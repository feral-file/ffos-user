#!/bin/sh
set -eu
media_prefix=${FF_MEDIA_PREFIX:-"$(pwd)/build-media/prefix"}
test "$(sha256sum vendor/ffmpeg-8.1.2.tar.gz | cut -d ' ' -f 1)" = 9fd092511605bbebafe095ea6d38d9e40f34d12f7386e1258372df8be0576eb7
mkdir -p vendor/ffmpeg build-media/ffmpeg "$media_prefix"
tar -xzf vendor/ffmpeg-8.1.2.tar.gz --strip-components=1 -C vendor/ffmpeg
cd build-media/ffmpeg
../../vendor/ffmpeg/configure --prefix="$media_prefix" --libdir="$media_prefix/lib" \
  --enable-shared --disable-static --enable-pic --disable-autodetect --disable-doc --disable-programs \
  --disable-debug --enable-gnutls --enable-vaapi --enable-libdrm --enable-zlib
make -j "${FF_BUILD_JOBS:-3}"
make install
