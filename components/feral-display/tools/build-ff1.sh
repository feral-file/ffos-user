#!/bin/sh
# Build the deployable CEF and libmpv runtime in the Linux toolchain.
set -eu
trial="$PWD/build-ff1"
mkdir -p "$trial/sdk"
archive="$trial/cef.tar.xz"
pin=maintenance/cef-ff1.json
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
  -DFF_WITH_CEF=ON -DCEF_ROOT="$trial/sdk" -DCMAKE_INSTALL_PREFIX=/usr
cmake --build "$trial/build" -j3
ctest --test-dir "$trial/build" --output-on-failure
stage=$(mktemp -d "$trial/stage.XXXXXX")
DESTDIR="$stage" cmake --install "$trial/build" --strip
# The SDK retains static symbols for debugging. Ship only the dynamic
# symbols needed by the loader. The pinned archive preserves the originals.
strip --strip-unneeded "$stage/usr/lib/feral-display/libcef.so"
# The FFOS launch path uses Chromium's root-owned sandbox helper.
sandbox="$stage/usr/lib/feral-display/chrome-sandbox"
test "$(stat -c %u:%g "$sandbox")" = 0:0
chmod 4755 "$sandbox"
root="$stage/usr/lib/feral-display"
sh tools/build-cage-client.sh
install -m644 build-cage-client/target/release/libff_wayland.so "$root/"
for license in LICENSE-MIT LICENSE-APACHE; do
  install -Dm644 "build-cage-client/licenses/$license" "$stage/usr/share/licenses/feral-display/wl-proxy/$license"
done
install -m644 maintenance/ffos-worker-packages.lock "$stage/usr/share/feral-display/"
python3 - "$stage/usr/share/feral-display/dependencies.json" "$pin" <<'PYTHON'
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
# Inspect the installed tree, including executable scripts outside lib/.
find "$stage/usr" -type f -perm /111 | sort | sed "s|$stage||" > "$trial/installed-executables.txt"
while IFS= read -r executable; do
  case "$executable" in
    /usr/lib/feral-display/feral-display|/usr/lib/feral-display/chrome-sandbox|/usr/lib/feral-display/chrome_crashpad_handler|*.so|*.so.*) ;;
    *) echo "Unexpected executable: $executable" >&2; exit 1 ;;
  esac
done < "$trial/installed-executables.txt"
# Archive only the install tree. mktemp makes the staging root mode 0700,
# which tar would otherwise apply to the installation prefix.
tar -czf "$trial/feral-display-linux.tar.gz" -C "$stage" ./usr
(cd "$trial" && sha256sum feral-display-linux.tar.gz > SHA256SUMS)
