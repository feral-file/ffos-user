# Native device presentation verification

The [shipping 3.0.1 comparison](PLAYER-UI-PARITY.md) updates the label layout, pointer, setup and pairing evidence for the direct Cage runtime.


Development increment, 2026-11-18. This is not activation or whole-player
acceptance. Homelab remains awake at Anh's request; the installed controller
still serves the legacy player until the remaining consumer migration is ready.

## Design and assets

`src/device_ui.cpp` renders device text and QR codes with Pango/Cairo. Only UI
changes create/upload a raster; artwork stays on the accelerated GPU path.
The PP Mori regular, bold and
bold-italic faces come from the existing ff-player product assets. WOFF was
discoverable by Fontconfig but Pango 1.58 substituted DejaVu while shaping it.
The checked-in SFNT copies preserve glyphs, metrics and layout tables; reproduce
them with `tools/unpack-ui-fonts.py` and fonttools 4.61.1. The test inspects the
actual Pango glyph-run face, not just Fontconfig's candidate match. At 92 px,
the pairing title measures 1,127 px; installed Chrome measured 1,127.02 px.
Font licensing remains the existing product's distribution responsibility.

The native panel follows SetupOverlay's short-edge font sizes, line heights,
82% scrim, rounded QR panel and two-module quiet margin. Claim copy follows the
newer installed homelab player. The exhibition title card is specified in [TITLE-CARD.md](TITLE-CARD.md).

The bundled setup artwork is the original p5 sketch and seed. Its two inline
scripts were extracted unchanged to permit strict CSP. Provenance and hashes
live with the assets. It gets a separate browser context, no renderer bridge,
no input, no playback callbacks and no history. One creation attempt per setup
episode bounds recovery. It fills empty setup screens and fades for 650 ms;
a setup panel over presented artwork keeps that artwork as its background.
A failed backdrop cannot remove the native QR/text panel.

## Hardware and evidence

Homelab: FFOS 3.0.0, kernel 6.18.38-2-lts, Radeon 680M/Rembrandt, Mesa 26.1.4,
Cage 0.3.1, 1920×1920 at 60 Hz. Production CEF pin: Chromium 156.0.8078.96.
Evidence: `evidence/ui-20261118/`. Captures are Cage compositor output or
explicit native raster fixtures, **not physical-panel verification**.

- Thirteen Linux CTest groups and ten macOS core groups passed. Linux includes
  font selection, label geometry, compact error notices and four panel sizes.
- Both claim and Wi-Fi payloads decoded exactly at 1920×1080, 1080×1920,
  1920×1920 and 3840×2160. The native compositor claim QR also decoded. All
  credentials in retained captures are synthetic.
- Eleven setup states plus clearing to ready passed socket/status/capture checks.
- Setup over live artwork created no decorative renderer. A four-second video
  remained committed, unpresented and paused for 65 seconds behind setup, then
  presented and advanced on EOF after clearing. Core tests also retain an EOF
  that races the initial pause and reject a retired occurrence's EOF.
- Thirteen media checks passed: 4K H.264 VA-API, 4096-square software fallback,
  pause/seek/resume, autoplay hold, EOF, HLS, AAC/MP3, volume/mute and bounded
  decoder crash/hang handling. PulseAudio connection does not prove audibility.
- EXIF orientation, animated GIF frames, alpha/aspect ratio and three GLB
  decode/input fixtures passed. These are regression fixtures, not visual
  equivalence for every artwork.
- Transparent Grit produced changing compositor output across two same-URL
  occurrences; 190 control round trips completed, median 47.96 ms, p95 165.16 ms,
  maximum 604.04 ms. This is a responsiveness regression, not a fair Chrome
  performance benchmark or a proof of performance parity.

## Backend finding and remaining scope

ANGLE `gl-egl` rendered the p5 setup lines black on this AMD device in both CEF
156.0.8078.96 and standalone Chrome for Testing 156.0.8078.80. Filled geometry
worked. Vulkan rendered the unmodified sketch in both engines and passed the
regressions above, so the native default is now ANGLE Vulkan. This selects the
WebGL backend; it does not enable Chromium's Vulkan compositor feature. Retain
the AMD nodcc workaround until separately qualified. The earlier matched-engine
pacing report used gl-egl and does not qualify Vulkan performance.

An initial visual comparison incorrectly compared the bundled sketch with
Transparent Grit behind the installed pairing panel. The different backgrounds
were different works, not evidence of a remaining Vulkan rendering defect.
The comparison did reveal the need to preserve live artwork under setup.

Normal phone casts/input, mint-pairing delivery, refresh/session/watchdog,
offline resource handling and coordinated FFOS activation remain unfinished.
Offline-degraded no-panel backdrop/chip behavior, physical rotation/high-DPI,
full portfolio, overnight playback and final same-engine Vulkan pacing still
need acceptance. Do not replace those checks with successful page navigation.
