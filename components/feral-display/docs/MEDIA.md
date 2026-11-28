# Native video and audio backend

Decision recorded 2026-11-17. Implementation is under verification, not release qualified.

Anh requested VLC or another alternative to stock CEF for video. Select libmpv 0.42.0 and libplacebo 7.387.0. The stock CEF 156 binary on FF1-SKYZ2E3A reports no H.264 playback support; its WebM and MP3 capability probes are positive. These probes establish format advertisement, not successful decoding.

## Why libmpv

| Candidate | Evidence | Decision |
|---|---|---|
| libVLC 3 | [Video callbacks](https://videolan.videolan.me/vlc-3.0/group__libvlc__media__player.html) use application buffers. Its documentation warns of additional copies and reduced hardware-decoding support for custom memory rendering. Native window embedding does not supply the required independent Wayland composition contract. | Do not choose the CPU callback path for 4K composition. |
| libmpv 0.42.0 | [Pinned render API](https://github.com/mpv-player/mpv/blob/v0.42.0/include/mpv/render_gl.h) supports FBO rendering and hardware-decoding interop with an EGL/native display. | Implement and measure on FF1. Do not infer zero-copy decoding from merely selecting `hwdec=auto-safe`. |

## Process and buffer ownership

The host spawns one worker for one occurrence through `posix_spawn`, passing a private `SOCK_SEQPACKET` descriptor. Only typed initialization, controls, frame acknowledgements and stop are accepted. No mpv JSON IPC socket, general mpv command, Lua/JS scripting, user configuration, terminal input or default key bindings are exposed.

The worker renders into an EGL texture and exports its DMA-BUF planes. It completes producer GPU work before sending the descriptors. The host imports the buffer, makes a GPU copy into its owned texture, completes that copy, and acknowledges the sequence before the worker can overwrite it. A separate `presented` acknowledgement follows the host swap; the copy acknowledgement is not presentation evidence. The packet cap is 64 KiB, at most four descriptors, and one frame may be outstanding. Pixel data is never carried over IPC. `glFinish` currently provides conservative synchronization; its pacing cost must be measured before replacing it with explicit native fences.

The worker has no playback database and cannot change the playlist. Decoder exit/hang becomes an occurrence-scoped host failure. Whole-GPU or host failures still require watchdog/systemd. The worker has a 3 GiB address-space limit and four FFmpeg decoding threads. The CPU-count default (up to 16) exhausted address space during 4096-square H.264 software fallback on FF1-SKYZ2E3A; four threads passed the same generated fixture without increasing the limit. This bounds reference-frame/thread allocation but can reduce peak software decoding throughput; sustained fallback frame pacing remains unqualified. The tested bubblewrap launcher hides the home directory, playback state, control sockets and D-Bus; it exposes only runtime libraries, temporary storage, TLS/font data, Wayland/PulseAudio sockets, GPU nodes and read-only GPU sysfs metadata. The test service contains host and children under a 6 GiB cgroup. The final service limit and capture contention still need qualification. A one-second worker heartbeat bounds a stopped worker to a five-second host-side timeout.

## Semantics and fidelity checks

- HTTP(S) progressive sources stream through libmpv/FFmpeg. Bounded demux buffers are 64 MiB forward and 16 MiB backward, with no full-file preload or disk cache. Offline URLs must come from the documented cache service; arbitrary local paths are not accepted by the worker.
- HLS and DASH are native streaming sources. Progressive, streaming, seekable and non-seekable cases need separate verification, including disconnect/reconnect and cold offline start. HLS VOD, progressive range requests, pause/resume/seek and audio routing have passed the short homelab checks; see `VERIFICATION.md`.
- `loop-file` repeats one media item when its display contract requires it. Host duration and playlist rules remain authoritative. EOF is an observation sent to the host, not permission for mpv to choose another file.
- Host pause, volume, mute and seek drive typed worker controls. Loading is not presentation: playback restart plus a compositable frame precede host-swap evidence.
- Hardware decoding, software fallback for unsupported dimensions, color range/matrix, HDR tone mapping, rotation, aspect ratio, frame pacing and audio synchronization require actual media evidence.
- Preserve the FFOS VCN 3.1.1 H.264 cap of 4096×4080. Test 4096×4096 H.264 explicitly; the kernel workaround is independent of which player library uses VA-API.

Software artworks keep their original documents and headers. This backend does not intercept or replace video elements inside arbitrary artist pages. That browser capability must be tested independently and may still require a codec-enabled CEF build.


Generate the full device fixture set before running the media suite:

```sh
python3 tools/make-media-fixtures.py /tmp/ff-native-media-fixtures
```

Copy that directory's contents alongside `tests/media-h264-aac.mp4` on the
test device, then serve it with `tools/test-media-server.py`. The generated
manifest records the installed FFmpeg CLI version, arguments and SHA-256 hashes;
encoding need not happen on the constrained FF1. The runtime uses the pinned
private decoder libraries, independently of the fixture encoder.


Untyped HTTP(S) sources without a recognized suffix are classified by an
occurrence-scoped CEF URL request before backend creation. HEAD is followed by
a bounded Range GET only if needed; the GET cancels on its first body callback
and the total probe deadline is three seconds. Superseded probes are canceled.
The observed MIME type selects a backend without rewriting the authored item
or its original URL. Failed classification becomes a visible incoming failure.
Cookie-authenticated media handoff to mpv and offline probe interception remain
unimplemented; these are not covered by the public HTTPS fixture results.

The media sandbox mounts both `/etc/ssl` and `/etc/ca-certificates` read-only
when present. Arch's SSL bundle links resolve into the latter directory. TLS
verification remains enabled; the device suite confirms an untrusted
self-signed HTTPS source fails before presentation.
