#!/bin/sh
set -eu
python3 tools/fetch-dependencies.py --without-cef
python3 tools/verify-vendor.py --without-cef
python3 tools/prepare-renderers.py
sh tools/build-presentation.sh
export PKG_CONFIG_PATH="$PWD/build-presentation/prefix/lib/pkgconfig:$PWD/build-presentation/prefix/share/pkgconfig"
export PATH="$PWD/build-presentation/prefix/bin:$PATH"
sh tools/build-ffmpeg.sh
sh tools/build-media.sh
export LD_LIBRARY_PATH="$PWD/build-media/prefix/lib:$PWD/build-presentation/prefix/lib"
export PKG_CONFIG_PATH="$PWD/build-media/prefix/lib/pkgconfig:$PWD/build-presentation/prefix/lib/pkgconfig:$PWD/build-presentation/prefix/share/pkgconfig"
cmake -S . -B build-ci -G Ninja -DFF_WITH_PUBLIC=ON -DCMAKE_BUILD_TYPE=RelWithDebInfo -DCMAKE_INSTALL_PREFIX=/usr
cmake --build build-ci -j3
ctest --test-dir build-ci --output-on-failure
python3 protocol/generate-schema.py --check
DESTDIR="$PWD/build-ci/stage" cmake --install build-ci
tar -czf build-ci/ff-player-native-linux.tar.gz -C build-ci/stage .
