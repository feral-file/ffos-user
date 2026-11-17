# Native player (`components/player`)

Native C++ player host for FFOS. Software artworks load as original top-level
CEF documents. Direct video/audio uses a separate, sandboxed libmpv worker.
Device UI, playback intent, history and the Unix control socket belong to the
host. This is an implementation under development, **not a qualified replacement
image**. See [the execution record](docs/PLAN.md) and [verification](docs/VERIFICATION.md).

## Build the core locally

Requires CMake 3.24+, C++20 and SQLite development headers. Fetching verifies all
dependency hashes, including the Linux engine; no source archive is trusted by
its filename alone.

```sh
python3 tools/fetch-dependencies.py
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
qrencode sqlite pango libdrm libglvnd wayland gnutls libva libpulse lcms2 curl libdisplay-info`.
The package inventory is evidence of the tested environment, not yet a fully
reproduced clean-image toolchain lock. The Ubuntu Dockerfile is a development
build only; its apt packages are not pinned for release.

```sh
python3 tools/fetch-dependencies.py
mkdir -p vendor/cef
tar -xjf vendor/cef.tar.bz2 --strip-components=1 -C vendor/cef
sh tools/build-ffmpeg.sh
sh tools/build-media.sh
export PKG_CONFIG_PATH="$PWD/build-media/prefix/lib/pkgconfig"
cmake -S . -B build -G Ninja -DFF_WITH_CEF=ON -DCMAKE_BUILD_TYPE=RelWithDebInfo
cmake --build build -j 3
LD_LIBRARY_PATH="$PWD/build-media/prefix/lib" ctest --test-dir build --output-on-failure
cmake --install build --prefix "$PWD/build-stage/usr"
```

`dependencies.json` pins CEF, libmpv, libplacebo, FFmpeg and their downloaded
source dependencies. The media build disables scripting, plugins and user
configuration. Staged shared media libraries stay private to the worker; they
do not replace FFOS system FFmpeg or libplacebo. The sandbox launcher requires
`bubblewrap`, access to the Cage Wayland socket and `/dev/dri`.

CEF's `chrome-sandbox` must be installed as root, mode 4755, in the runtime
directory. Do not use `--no-sandbox`. Packaging still needs its coordinated FFOS
activation, dependency/license audit and rollback verification.

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
  --use-angle=gl-egl --enable-features=Vulkan --enable-unsafe-webgpu
```

`AMD_DEBUG=nodcc` is the measured workaround for CEF #4354, not a general
recommendation for Chromium. `--use-gl=egl` is incompatible with the selected
CEF build; use the pinned ANGLE path above. WebGPU permission and driver
qualification remain portfolio checks. Remote debugging is opt-in diagnostic
configuration, never required for product commands.

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

`.github/workflows/build-player.yaml` builds the Linux CEF host and private
FFmpeg/libplacebo/libmpv libraries, runs CTest, and retains a staged runtime
archive on feature-branch pushes and pull requests. It does not publish or
activate packages. The Ubuntu container validates portable compilation; the
pinned FFOS pacman build remains the production packaging path. See
`docs/BENCHMARK.md` for the unresolved Transparent Grit presentation failure.
