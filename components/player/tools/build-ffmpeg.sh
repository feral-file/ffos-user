#!/bin/sh
set -eu
media_prefix=${FF_MEDIA_PREFIX:-"$(pwd)/build-media/prefix"}
test "$(sha256sum vendor/ffmpeg-8.1.3.tar.xz | cut -d ' ' -f 1)" = 7138d28c96d9d3e3af4ee3d8cad72741f8ffb40da90c1112235dea3ecd3178a3
mkdir -p vendor/ffmpeg build-media/ffmpeg "$media_prefix"
tar -xf vendor/ffmpeg-8.1.3.tar.xz --strip-components=1 -C vendor/ffmpeg
cd build-media/ffmpeg
../../vendor/ffmpeg/configure --prefix="$media_prefix" --libdir="$media_prefix/lib" \
  --enable-shared --disable-static --enable-pic --disable-autodetect --disable-doc --disable-programs \
  --disable-debug --enable-gnutls --enable-vaapi --enable-libdrm --enable-zlib --enable-libdav1d
make -j "${FF_BUILD_JOBS:-3}"
make install
