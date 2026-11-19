# Engine ownership and update procedure

The normal player now uses the separate public Chromium adapter. Its exact pin
is Chrome for Testing 158.0.8127.36; `dependencies.json` records archive and
executable hashes, and `maintenance/public-engine-update.json` records the
156-to-158 integration rehearsal. [PUBLIC-ENGINE.md](PUBLIC-ENGINE.md) is the
current architecture and qualification record. CEF builds below are retained
only as the explicit pre-cutover rollback/earlier-trial record.

For a public-engine update:

1. Read the official Chrome for Testing stable manifest. Record its timestamp,
   release, download URL, archive hash and unpacked executable hash. Validate
   the archive before extracting. Observe Browser.getVersion and the actual
   headful user agent on the authorized FF1.
2. Update the dependency pin, fetch and verify all source inputs, then rebuild
   the native host/package and run Linux CTest, Go tests/vet/lint, schemas,
   interoperability and script contracts. Keep the prior runtime/state backup.
3. Run native raster/video/audio, SVG, original-URL WebGL/WebGPU, bundled GLB,
   native UI/input, cached replay/cold restart, process isolation and recovery
   checks. Recheck the worker Fetch-domain limitation on the new engine; do not
   infer support from a cached worker script. Re-run matched-engine/seed pacing.
4. Carry or retire each workaround explicitly. There are no applied engine
   patches in the public adapter. A future patch needs the exact upstream base,
   complete diff, purpose, regression test, update and retirement procedure.
5. Stage a fresh public-only install tree, with no stale CEF libraries. Record
   installed executable hashes and verify engine identity after cutover. Retain
   compositor captures separately from physical panel evidence. Broader corpus,
   overnight, distribution licensing/symbol and clean-build reproducibility
   checks remain release qualification work. Prepare a coordinated full-image
   release; promotion and publication remain human operated.

## Earlier CEF trial and rollback record


This is an implementation/maintenance record, not security-update acceptance.
The previous-stable/current-stable build rehearsal passed both eleven-test Linux suites. Full portfolio qualification remains pending. The Transparent Grit host scheduling/buffer regression and its verification are recorded in BENCHMARK.md.

`dependencies.json` is the source of exact engine/library versions, download
locations and SHA-256 hashes. The CEF pin is 156.0.30, Chromium 156.0.8078.96,
Linux x86-64. Direct video/audio use the separately pinned libmpv/FFmpeg worker;
an engine update must not silently move those files back into browser media.
Video inside an artist's software document retains that document's browser
semantics. Its codec coverage is a separate outstanding corpus check.

## Current engine-specific constraints

| Constraint | Purpose and evidence | Removal/update test |
|---|---|---|
| `AMD_DEBUG=nodcc` | Workaround for accelerated rendering regression [CEF #4354](https://github.com/chromiumembedded/cef/issues/4354), reproduced on FF1 AMD hardware | Compare patched/upstream engine with and without it on holz, Living Code, GLB and media; capture GPU resets, memory and physical output |
| `--use-angle=vulkan` (default) | ANGLE GL drops p5 WebGL lines on this AMD device in both CEF/Chrome 156. Vulkan renders the original setup sketch and passes media/image/GLB/Grit regressions; see DEVICE-UI.md | Recheck both backends against the unmodified sketch and full portfolio on engine/driver updates. Earlier gl-egl pacing measurements do not qualify Vulkan performance |
| `NO_STACK_PROTECTOR` on process entry | CEF zygote changes the stack canary; [CEF #3912](https://github.com/chromiumembedded/cef/issues/3912) | Build using package hardening flags and exercise renderer forks/restarts |
| Implicit DMA-BUF producer readiness | Mesa orders imported reads after producer fences; the host completes its GPU copy during the borrowed callback. External BeginFrames and per-turn admission prevent producer/callback backlog | `device-render-loop-check.mjs`, static image/input/GLB readiness, copy/pacing counters. [CEF #4313](https://github.com/chromiumembedded/cef/issues/4313) concerns missing NVIDIA producer fences; host polling does not repair missing fences |
| Per-context bundled scheme factory | Global factory registration did not serve independently created request contexts | Image and plain/Draco/KTX2 GLB load under fresh contexts; artist contexts cannot navigate to the bundled renderer |

There is no applied local Chromium patch yet. Do not describe the AMD workaround
or the host's synchronization code as an engine fix. If a focused patch is
required, retain its complete diff under `patches/cef/<base-version>/`, with the
upstream commit/issue, original and patched revisions, rationale, regression
fixture and retirement condition in an adjacent manifest. Apply with `git apply
--check` against the exact pinned source before building. Never regenerate a
patch against a different engine and assume its behavior survived.

## Update sequence

1. Record the old source bundle hash, engine pin, installed package hashes and
   isolated-state backup. Save the current device baseline and per-work results.
2. Select a supported upstream release from the official CEF build index. Record
   Chromium/CEF versions and API hashes. Fetch into a new directory and verify
   the archive hash. A beta binary can be used for diagnosis; it is not silently
   substituted for the production stable pin.
3. Review upstream security/release changes, API changes and each local patch or
   workaround. Carry or retire each explicitly. Review codec coverage separately
   from WebGL/WebGPU and Wayland composition. Update `dependencies.json` and the
   toolchain/dependency lock together where the new engine requires it.
4. Build a new source bundle and pacman package in the pinned FFOS environment.
   Run core, protocol/schema, Go/C++ interoperability, migration, media IPC and
   decode tests. Retain `.BUILDINFO`, source manifest, build log and packages.
   Compare two clean builds before claiming binary reproducibility.
5. Install on the authorized FF1 without activating the coordinated production
   units. Run media/input/recovery/model tests, holz and every Living Code work,
   cached-resource/worker cases, browser/GPU failure paths and cold restart.
   Compare frame pacing, dropped frames, process/GPU memory and physical output
   against the previous engine on the same hardware. Repeat the overnight run.
6. Update evidence and the patch manifest, run fresh-context review, and prepare
   the coordinated full-image release declaration. Promotion/publishing remains
   human-operated under ffos/ffos-user rules. Retain the old package, old runtime
   and pre-cutover state export for the documented rollback.

## Diagnostics and licenses

Native host/library debug packages are emitted by makepkg. The current upstream
minimal CEF archive references DWO objects absent from that archive; a successful
debug-package build does **not** establish complete CEF symbols. The retained build index lists no separate symbol archive for this pin. Obtain
matching symbols from a reproducible source build (or a verified upstream
artifact if one is published) before release qualification. Record build
IDs with crash reports so symbols cannot be matched by filename alone.

CEF's bundled license, model-viewer/Draco/Basis notices, FFmpeg LGPL, libmpv LGPL
and copyright notices, libplacebo and the JSON-header notice are installed with
the runtime. Runtime/system dependencies still require a complete distribution
license/source-offer audit. The new host repository's license declaration has
not been selected; the private development package must not be represented as
having a completed public-distribution license audit.

### WebGPU composition on the installed FFOS session

On CEF 156.0.8078.96, ANGLE Vulkan alone let holz navigate and compose a black
canvas. Chromium reported `SharedImageBackingFactory` missing for a WebGPU swap
texture. The session now also enables `Vulkan,VulkanFromANGLE` and disables the
Vulkan presentation surface (the host owns the Wayland surface). The same URL
then produced visible geometry in a compositor capture. This is an observed
release/device configuration, not an assumption from upstream defaults.
[Chromium GPU initialization](https://chromium.googlesource.com/chromium/src/+/refs/heads/main/content/browser/gpu/fallback.md)
distinguishes ANGLE/Vulkan initialization from Vulkan compositing; the
[WebGPU implementation guidance](https://github.com/gpuweb/gpuweb/wiki/Implementation-Status)
lists the Linux interop features. Recheck WebGL, WebGPU, image and media paths
on each engine/driver update. This change does not establish frame-pacing parity.
