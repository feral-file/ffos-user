#!/bin/sh
# Run in the Linux player toolchain image. Build the FF1 trial separately from
# the shipping public engine so each artifact has an unambiguous runtime.
set -eu
trial="$PWD/build-cef-trial"
mkdir -p "$trial/sdk"
archive="$trial/cef.tar.xz"
curl --fail --location --retry 3 -o "$archive" \
  https://github.com/obs-rust/cef/releases/download/8078/cef_binary_8078_linux_x86_64.tar.xz
echo "fb36df11bc8280c9cb666732becc1fe267f833bb964dbb13d69cad33488171dc  $archive" | sha256sum -c -
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
tar -czf "$trial/ff-player-cef-linux.tar.gz" -C "$trial/stage" .
