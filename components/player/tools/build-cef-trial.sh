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
sh tools/build-ffmpeg.sh
sh tools/build-media.sh
export PKG_CONFIG_PATH="$PWD/build-media/prefix/lib/pkgconfig"
export LD_LIBRARY_PATH="$PWD/build-media/prefix/lib"
cmake -S . -B "$trial/build" -G Ninja -DCMAKE_BUILD_TYPE=Release \
  -DFF_WITH_CEF=ON -DCEF_ROOT="$trial/sdk" -DCMAKE_INSTALL_PREFIX=/usr
cmake --build "$trial/build" -j3
ctest --test-dir "$trial/build" --output-on-failure
DESTDIR="$trial/stage" cmake --install "$trial/build" --strip
# The FFOS launch path uses Chromium's root-owned sandbox helper.
sandbox="$trial/stage/usr/lib/ff-player-native/chrome-sandbox"
test "$(stat -c %u:%g "$sandbox")" = 0:0
chmod 4755 "$sandbox"
python3 - "$trial/stage/usr/share/ff-player-native/dependencies.json" "$pin" <<'PYTHON'
import json
from pathlib import Path
import sys
manifest, trial_pin = map(Path, sys.argv[1:])
dependencies = json.loads(manifest.read_text())
dependencies["cef"] = json.loads(trial_pin.read_text())
dependencies.pop("chromium")
manifest.write_text(json.dumps(dependencies, indent=2) + "\n")
PYTHON
tar -czf "$trial/ff-player-cef-linux.tar.gz" -C "$trial/stage" .
