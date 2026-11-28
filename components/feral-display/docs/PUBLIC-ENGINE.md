# Public engine integration and evidence

Work started at ffos-user 06477a263089a231bdd17366a802a37a88004b01 and ffos
bf937dbb5c55f3fc043eec67b6012ec0bddbc14e. Both owning trees were clean.
Unrelated ff-player changes were preserved. Work stays on codex/native-player;
no PR, promotion or publishing workflow is authorized by this checkpoint.
Capsule is deferred. FF1-SKYZ2E3A is the hands-on trial device. The
[FF1 cutover report](PUBLIC-ENGINE-FF1.md) records the current installation.

## Ownership and isolation

The production `feral-display` executable does not link libcef. Each software
occurrence gets a separate public Chromium browser process, fresh profile and
private Wayland server. Chromium can share renderers within that browser; the
separate browser processes/profiles are the isolation boundary we create and
verify. Artist documents load at their original URLs, outside an iframe shell.
Native labels, QR/setup narration, cursor, loading/error UI and composition
never live in an artist document. GLB alone uses the trusted bundled viewer.
Raster images use the native media worker; SVG keeps browser semantics.

Artwork changes use the original player's CSS `ease` curve. The default
650 ms crossfade overlaps native raster and media. A change involving a
software document, SVG, or GLB fades out for 650 ms and then fades in for
650 ms. The native host starts only when it has a presentable incoming frame.
The outgoing artwork owns presentation during the first half of a sequential
fade. The incoming occurrence enters history on the first submitted frame
with nonzero opacity. The previous stage stays in place during the crossfade.
The media worker renders the incoming work at that stage's crop and margin,
with source alpha and transparent letterboxes. The compositor paints the
shared matte. Status reports the observed stage and temporarily rejects
showing-specific overrides. The final stage is published after a frame at that
configuration is submitted. Policy and source refreshes retire retained
outgoing pixels.
Setting `transitionMs` to zero selects a cut. Layout and matte switch together
when the matching frame is ready. The old web player's separate 200 ms
padding/background animation is outside this opacity port.

An isolated FF1-SKYZ2E3A test checked blended raster pixels, a raster-to-web
fade through black, and a zero-duration cut. A fit-to-fill case checked a
retained white matte and a blue final edge. A covered handoff retained the
last exposed matte across a change in settings. Transparent overlap preserved
more of the outgoing image behind the transparent half than behind the opaque
half. A policy change retired a mature outgoing fixture mid-fade. The captures
sampled Cage compositor pixels.
[Transition evidence](evidence/legacy-transition-20261119/compositor-pixels.json)

A focused C wrapper owns a headless wlroots scene, including Chromium's child
surfaces. Its DMA-BUF export is imported into the native SDL/EGL host. Normal
artwork composition has no CPU readback. Scene output-sample events count nested
GPU composition passes separately from the host's GPU copy. UI rasterization is
native Pango/Cairo and is a separate low-frequency path. A retained source stays locked until a polled GPU copy fence completes;
ordinary UI redraw cannot acknowledge a newer in-flight source. The private
output permits Chromium to render ahead once a copy is queued. It reports Cage
presentation separately, with a bounded fallback and a counter. The shared GPU/driver can
still stall or kill the host; watchdog/systemd own that process-level boundary.

Each browser's inherited private engine pipe carries only typed browser-engine
operations: navigation/lifecycle, input, permission/download policy, health,
request continuation and captured-response fulfillment. Controld never uses that
pipe or a debugging TCP port for product control. There is no handleCDPRequest
or arbitrary evaluation operation in the native player protocol. An explicit
operator debugger remains opt-in. Popups/downloads/permission prompts cannot
replace device controls. A renderer hang is probed without evaluating artist JS.

Pointer/button/wheel events use the private native seat, with authoritative host
coordinates and modifiers. Raw physical keyboard events and UTF-8 text remain
separate, avoiding duplicate phone characters. Focus loss/view replacement
releases held state. Public browser profiles retire asynchronously after their
process exits, with bounded cleanup and a startup sweep.

## Cache and connectivity

[protocol/CACHE.md](../protocol/CACHE.md) specifies the same-UID resource API.
Controld keeps its store and trust decisions. Browser response replay preserves
original URLs, repeated CSP and other applicable headers. It does not call entry
HTML alone offline-ready. Unsupported variants, missing/corrupt resources and
legacy policy captures are explicit failures/notices. Browser fulfillment has a
32 MiB response limit; opaque native media is classified before body fulfillment; native files are streamed/searched through read-only FDs
without base64 or full-file buffering. Software with larger browser dependencies
still needs a streaming fulfillment engine extension before portfolio acceptance.

Production browser and media workers run in private network namespaces. Online
workers reach HTTP(S) through an occurrence-local proxy. The host proxy resolves
absolute hostnames, rejects non-public addresses, and dials the checked IP only
when Linux selects a default route. More-specific routes (including routed LAN
prefixes) fail closed by default. The global localNetwork grant permits unicast LAN routes. Offline workers have no proxy. The player applies saved browser grants before navigation. Geolocation defaults to denied. An Internet server still sees the
device's public IP. Operator CDP diagnostics use the namespace relay in
`tools/diagnostic-netns-proxy.py` installed as `/tmp/ff-diagnostic-netns-proxy.py`.

Offline namespaces prevent silent live fallback. Runtime status exposes cache
hits/misses/coverage, and a native notice reaches both the screen and controld's
external status projection. A real online edge can restart a failed/incomplete
offline occurrence; healthy work is not restarted merely because a verdict moved.
Onchfs virtual-origin response policy stays owned by controld, independently of
the deferred exact-engine capsule. Capture Chromium follows the pinned public
engine's update lifecycle, rather than an unrelated system binary by default.

## Phone delay

The inspected phone casts spent 34.6106 and 34.6134 seconds in controld before
replying. Catalog series expansion ran sequentially for a 30-second budget,
its HTTP Get ignored the operation context, and source preflight added 2–4
seconds. The fix binds catalog requests to context, limits optional interactive
series expansion to two seconds and preflight to one second. Unresolved series
retain their representative; background refresh keeps its longer budget.

On the same device, the signed FF-CLI URL cast for playlist
`ecadeee2-743c-4821-ab9c-7d8bf897cf76` returned HTTP 200/accepted in 2.289756077
seconds after the fix, versus 34.613442617 seconds in the phone log before it.
This is command admission timing, not artist-render or physical-display proof.
Private phone addresses, control IDs and encrypted envelopes are not published.

## Verification checkpoint

Observed on FF1-SKYZ2E3A during integration:

* A private socket-path compositor probe delivered 562 DMA-BUF frames in 20
  seconds without its former GPU crash. Passing WAYLAND_SOCKET by descriptor
  was invalid across Chromium's child FD remapping; private paths fixed it.
* Integrated holz reached host composition with native title/artist/exhibition
  labels; a Cage capture was inspected. This is not a photograph of the panel.
* Integrated network-isolated holz replay observed seven hits and one miss.
  Imported coverage remained incomplete. The miss is retained, not counted as
  full offline acceptance.
* Native JPEG EXIF rotation, PNG straight alpha/contain framing, and both GIF
  animation colors passed compositor pixel checks. Backend/status showed native
  decode, not a browser document. GLB Astronaut reached presentation using the
  pinned bundled viewer; visual/equivalence checks remain separate.
* Extensionless MP4 handed off to the native worker on158; SVG stayed a
  browser document. The Go gesture suite and host-input suite passed, including
  Unicode, double clicks, wheel magnitude, Enter form submission, disconnect
  release and stale-occurrence rejection.

The [FF1 cutover report](PUBLIC-ENGINE-FF1.md) has the installation evidence and
the matched public-engine comparison. The earlier CEF comparison used a different
composition path and engine pin.

## Corrected integration observations

The same original-URL synthetic document replayed while the device WAN verdict
was online and its live URL was absent. The broker selected its manual capture
and the browser ran in a separate network namespace. Fonts, WASM returning42,
a same-origin nested frame, redirect, page runtime fetch and dedicated-worker
runtime fetch all passed. A deliberately restrictive CSP initially blocked WASM;
adding wasm-unsafe-eval to the fixture's own recorded policy allowed it. Replay
preserved that policy rather than disabling browser enforcement.

Chromium158 does not expose Fetch.enable on worker targets, but that alone did
not predict dedicated-worker failure: the measured requests replayed through
the page's interception path. Service-worker registration remained unavailable.
That precise limitation and incomplete coverage are exposed to the device and
sender. Broader worker/service-worker acceptance is still open.

## Embedded H.264 regression check

On homelab (Chrome for Testing 158.0.8127.36, RADV Rembrandt, FFOS kernel
6.18.38-2-lts), the public launcher needed both `DefaultANGLEVulkan` and
`VaapiIgnoreDriverChecks` in addition to `Vulkan,VulkanFromANGLE`.
With either added feature alone, embedded 3840×2160 H.264 selected
`FFmpegVideoDecoder`. With both, it selected `VaapiVideoDecoder`.

Repeat after changing the browser build or flags. Serve the generated
`media-4k-h264.mp4` and `media-4096-square-h264.mp4` fixtures over HTTP.
In a test document running under the normal public launcher, play each URL
in a muted HTML video element. In DevTools' Media panel, check the decoder
properties for each player while its playback clock advances:

| H.264 fixture | kVideoDecoderName | kIsPlatformVideoDecoder |
|---|---|---|
| 3840×2160 at 30 fps | VaapiVideoDecoder | true |
| 4096×4096 at 2 fps | FFmpegVideoDecoder | false |

The FFOS VCN 3.1.1 driver caps hardware H.264 at 4096×4080. The square clip
checks software fallback. Casting these MP4 URLs directly exercises libmpv,
so use the embedded fixtures for this regression.
