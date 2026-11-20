#!/bin/sh
# Run in the Linux player toolchain image. Build the FF1 trial separately from
# the shipping public engine so each artifact has an unambiguous runtime.
set -eu
trial="$PWD/build-cef-trial"
mkdir -p "$trial/sdk"
archive="$trial/cef.tar.xz"
pin=maintenance/cef-trial.json
url=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["url"])' "$pin")
digest=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["sha256"])' "$pin")
curl --fail --location --retry 3 -o "$archive" "$url"
echo "$digest  $archive" | sha256sum -c -
tar -xf "$archive" -C "$trial/sdk" --strip-components=1
python3 tools/fetch-dependencies.py --without-cef
python3 tools/verify-vendor.py --without-cef
sh tools/build-presentation.sh
export PATH="$PWD/build-presentation/prefix/bin:$PATH"
export PKG_CONFIG_PATH="$PWD/build-presentation/prefix/lib/pkgconfig:$PWD/build-presentation/prefix/share/pkgconfig"
sh tools/build-ffmpeg.sh
sh tools/build-media.sh
export PKG_CONFIG_PATH="$PWD/build-media/prefix/lib/pkgconfig:$PKG_CONFIG_PATH"
export LD_LIBRARY_PATH="$PWD/build-media/prefix/lib:$PWD/build-presentation/prefix/lib"
cmake -S . -B "$trial/build" -G Ninja -DCMAKE_BUILD_TYPE=Release \
  -DFF_WITH_PUBLIC=ON -DFF_WITH_CEF=ON -DFF_PACKAGE_CEF_VIEWS=ON -DCEF_ROOT="$trial/sdk" -DCMAKE_INSTALL_PREFIX=/usr
cmake --build "$trial/build" -j3
ctest --test-dir "$trial/build" --output-on-failure
stage=$(mktemp -d "$trial/stage.XXXXXX")
DESTDIR="$stage" cmake --install "$trial/build" --strip
DESTDIR="$trial/probe" cmake --install "$trial/build" --component diagnostic-probes --strip
install -m755 "$trial/probe/usr/lib/ff-player-native/cef-views-probe" \
  "$stage/usr/lib/ff-player-native/ff-cef-views-engine"
# The SDK retains static symbols for debugging. Ship only the dynamic
# symbols needed by the loader. The pinned archive preserves the originals.
strip --strip-unneeded "$stage/usr/lib/ff-player-native/libcef.so"
# The FFOS launch path uses Chromium's root-owned sandbox helper.
sandbox="$stage/usr/lib/ff-player-native/chrome-sandbox"
test "$(stat -c %u:%g "$sandbox")" = 0:0
chmod 4755 "$sandbox"
# bwrap exposes only engine/ to artwork. Hard links let both the offscreen
# host and the windowed worker use the exact same CEF binaries on disk.
root="$stage/usr/lib/ff-player-native"
mkdir "$root/engine"
for src in "$trial/sdk/Release/"* "$trial/sdk/Resources/"*; do
  name=$(basename "$src")
  cp -al "$root/$name" "$root/engine/$name"
done
ln "$root/ff-cef-views-engine" "$root/engine/chrome"
python3 - "$stage/usr/share/ff-player-native/dependencies.json" "$pin" <<'PYTHON'
import json
from pathlib import Path
import sys
manifest, trial_pin = map(Path, sys.argv[1:])
dependencies = json.loads(manifest.read_text())
dependencies["cef"] = json.loads(trial_pin.read_text())
dependencies.pop("chromium")
dependencies["cefViews"] = {
    "version": dependencies["cef"]["version"].split("+chromium-")[1],
    "observedUserAgent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36",
}
manifest.write_text(json.dumps(dependencies, indent=2) + "\n")
PYTHON
tar -czf "$trial/ff-player-cef-linux.tar.gz" -C "$stage" .
