# Native player (`components/player`)

The FF1 runtime uses a C++ playback host under Cage. Controld delivers signed
DP-1 playlists to the host over its Unix socket. The host owns scheduling,
SQLite state, labels, device panels, cursor routing and worker recovery.

| Artwork | Worker |
| --- | --- |
| Raster image, including animated GIF and AVIF | Sandboxed libglycin loader, native GL presentation |
| Direct video and audio | libmpv with FFmpeg, VA-API when supported, software decode otherwise |
| SVG, website and software artwork | CEF Views, original document URL |
| glTF and GLB | CEF Views with the bundled model-viewer |

Eligible fullscreen workers submit buffers to Cage. The host composites media
during transitions and when native device UI requires it. Browser handoffs that
need native composition keep the same composited document after the fade. The
public Chromium build remains available for same-device comparisons.

GitHub Actions builds the FF1 CEF archive with `tools/build-ff1.sh`.
The FFOS package recipe accepts that archive and its tested SHA-256. The CEF
version and download hash are pinned in `maintenance/cef-ff1.json`.
See [homelab playback checks](docs/CAGE-WORKERS.md) and the
[shipping-player baseline](docs/HOMELAB-PARITY.md).

## Build the core locally

Requires CMake 3.24+, C++20 and SQLite development headers. Fetching verifies all
dependency hashes, including the Linux engine; no source archive is trusted by
its filename alone.

```sh
python3 tools/fetch-dependencies.py --without-cef
cmake -S . -B build-core -DCMAKE_BUILD_TYPE=Debug
cmake --build build-core -j 3
ctest --test-dir build-core --output-on-failure
```

The core executable has no display backend. It exists for protocol/state tests.
It must never be installed as the production presentation service.

## Build the FFOS Linux runtime

The tested native environment is recorded in `docs/evidence/homelab-packages.txt`:
FFOS 3.0.0, Linux x86-64, GCC 16.1.1, CMake 4.4.0, Meson 1.11.2 and Ninja 1.13.2.
Use the FFOS pacman snapshot `2026/07/13`. Required build packages include
`gcc make cmake ninja meson pkgconf python-jinja nasm sdl2-compat libass
qrencode sqlite pango libdrm libglvnd wayland wayland-protocols gnutls libva libpulse lcms2 curl libdisplay-info`.
The package inventory is evidence of the tested environment, not yet a fully
reproduced clean-image toolchain lock. The Ubuntu Dockerfile is a development
build only; its apt packages are not pinned for release.

```sh
python3 tools/fetch-dependencies.py --without-cef
sh tools/build-presentation.sh
export PKG_CONFIG_PATH="$PWD/build-presentation/prefix/lib/pkgconfig"
sh tools/build-ffmpeg.sh
sh tools/build-media.sh
export PKG_CONFIG_PATH="$PWD/build-media/prefix/lib/pkgconfig:$PWD/build-presentation/prefix/lib/pkgconfig"
cmake -S . -B build -G Ninja -DFF_WITH_PUBLIC=ON -DCMAKE_BUILD_TYPE=RelWithDebInfo
cmake --build build -j 3
LD_LIBRARY_PATH="$PWD/build-media/prefix/lib:$PWD/build-presentation/prefix/lib" ctest --test-dir build --output-on-failure
cmake --install build --prefix "$PWD/build-stage/usr"
```

`dependencies.json` pins the public Chromium archive/executable, wlroots,
libmpv/libplacebo/FFmpeg, viewer and build SDK archives. The private wlroots build
uses only headless/Wayland and GLES composition; it cannot take the physical DRM
output from Cage. Its ABI does not depend on a future FFOS wlroots package.
Media libraries and the browser engine stay private to this component. They do
not replace FFOS system graphics/media packages.

The browser's `engine/chrome_sandbox` is installed root:root, mode 4755 by the
pacman recipe. Chromium's sandbox remains enabled. Bubblewrap separates each
browser profile and private Wayland socket from the host's state, D-Bus and
command socket. Offline browser/decoder processes have no live network.

The old CEF backend is an explicit `FF_WITH_CEF=ON` rollback/diagnostic build,
named `ff-player-cef-rollback`; it is not the production target. Do not activate
two normal playback owners. See the [local broker contract](protocol/CACHE.md).

## Isolated device test

Use separate state and socket directories. The socket parent must be owned by
the runtime UID and mode 0700. Do not point this build at an existing Chromium
profile. `tools/legacy-export.mjs` exports through the old player's live storage
APIs. `tools/migrate-legacy.py` validates and atomically imports into a new native
state directory; see [migration and rollback](docs/MIGRATION.md) for supported
records, refusal cases and the still-incomplete coordinated activation.

```sh
AMD_DEBUG=nodcc SDL_VIDEODRIVER=wayland \
  /path/to/stage/usr/lib/ff-player-native/ff-player-native \
  --state-dir=/home/feralfile/.state/native-test \
  --socket=/run/user/1000/ff-native-test/player.sock \
  --renderer-dir=/path/to/stage/usr/share/ff-player-native/renderers \
  --diagnostic-presentation-timing=/tmp/native-presentation.json
```

The launcher's engine flags are a tested, fixed configuration, not artist input.
The AMD DCC workaround still needs retirement evidence. Use
`--diagnostic-engine-debugger` only for an operator diagnostic session: Chromium
writes its ephemeral loopback port to that occurrence's `DevToolsActivePort`.
Normal playback has no debugging TCP port. Product commands always use the
native protocol; arbitrary evaluation is not a player API.

For systemd tests, use `KillMode=mixed` and a bounded stop timeout. Remove
`DBUS_SESSION_BUS_ADDRESS` from the child environment so Chromium cannot move
itself into an uncapped app scope. Keep the existing services installed until
the coordinated migration is verified.

```sh
python3 tools/playerctl.py --socket /run/user/1000/ff-native-test/player.sock state.get
```

The destructive hardware test `tools/device-media-check.py` requires the fixture
server and an isolated runtime. It kills/stops decoder children intentionally.
It is not an ordinary controller command or a test to run against a wall that
is serving an exhibition.

## Development CI

`.github/workflows/build-player.yaml` builds the Linux public Chromium host and private
FFmpeg/libplacebo/libmpv libraries, runs CTest, and retains a staged runtime
archive on feature-branch pushes and pull requests. It does not publish or
activate packages. The Ubuntu container validates portable compilation; the
pinned FFOS pacman build remains the production packaging path. See
`docs/PUBLIC-ENGINE.md` for current device evidence and remaining boundaries.

Frame-pacing measurements and the exact-engine/seed comparison are documented in
[the pacing report](docs/PACING.md). `--diagnostic-presentation-timing=/absolute/path.json`
writes bounded compositor timestamp samples on graceful shutdown. Normal logs
contain aggregate timing only. These are compositor observations, not panel
photographs or artwork correctness attestations. The public launcher's Vulkan/ANGLE feature set is checked on FF1 AMD hardware;
recheck it on engine/driver updates. WebGPU qualification remains per artwork.

## FF1 runtime ownership and upgrade checks

The host, native media worker and CEF worker build from this component. The
host owns the control socket, layout, tombstone, QR and pointer. The Cage
overlay is a host-owned Wayland surface. The compositor applies the physical
output transform once for every surface. Image EXIF and media orientation
metadata still apply within each artwork.

The **Build FF1 player** workflow produces `ff-player-native-linux-x86_64`
with the runtime archive and `SHA256SUMS`. Extracting the archive preserves
the CEF sandbox helper and the hard-linked engine files. The separate
Chromium comparison workflow produces `ff-player-chromium-linux-x86_64`.

CEF has its own FF1 archive digest. FFmpeg and mpv source digests live in
`dependencies.json`. The Cage relay builds from a verified wl-proxy revision
and its Cargo.lock. Ubuntu build packages for glycin are pinned in the
Dockerfile. The installed `ffos-worker-packages.lock` records the device
packages independently. Before qualifying a device image, run this from
the installed `/usr/share/ff-player-native` directory:

```sh
pacman -Q $(awk '{print $1}' ffos-worker-packages.lock) | diff -u ffos-worker-packages.lock -
```
