#!/bin/sh
set -eu
# Run inside the pinned build container, from the source root. No implicit
# meson downloads or user configuration. Archives are checked before extraction.
media_prefix=${FF_MEDIA_PREFIX:-"$(pwd)/build-media/prefix"}
mkdir -p "$media_prefix"
export PKG_CONFIG_PATH="$media_prefix/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
test "$(sha256sum vendor/mpv-0.42.0.tar.gz | cut -d ' ' -f 1)" = bcfebbfa2b65a6d88dc97ea18117831072373e38b5785a2afb874ed4df5070dd
test "$(sha256sum vendor/libplacebo-7.387.0.tar.gz | cut -d ' ' -f 1)" = f9e62d3774a9c4cfd198ec3abaa88294aaade2e31c04d5ab02bcf4abc98b1df5
tar -xzf vendor/mpv-0.42.0.tar.gz -C vendor
tar -xzf vendor/libplacebo-7.387.0.tar.gz -C vendor
test "$(sha256sum vendor/glad.tar.gz | cut -d ' ' -f 1)" = 33dbeae44d8315ece57e14eba4b1b4a02ac5406d1c4f49cd20048c91522a6b9a
tar -xzf vendor/glad.tar.gz --strip-components=1 -C vendor/libplacebo-7.387.0/3rdparty/glad
test "$(sha256sum vendor/fast_float.tar.gz | cut -d ' ' -f 1)" = 2b132274539286e41f37857cac22aa8441d21bd86d55de825a3342b149f66801
tar -xzf vendor/fast_float.tar.gz --strip-components=1 -C vendor/libplacebo-7.387.0/3rdparty/fast_float
test "$(sha256sum vendor/vulkan-headers.tar.gz | cut -d ' ' -f 1)" = 75967a8288ff16044a86d1af462b96d2e75c435937efb75f2bb4102ab0382f29
mkdir -p vendor/vulkan-headers "$media_prefix/include"
tar -xzf vendor/vulkan-headers.tar.gz --strip-components=1 -C vendor/vulkan-headers
cp -R vendor/vulkan-headers/include/. "$media_prefix/include/"
export CFLAGS="${CFLAGS:-} -I${media_prefix}/include"
export CXXFLAGS="${CXXFLAGS:-} -I${media_prefix}/include"
meson setup --wipe build-media/placebo vendor/libplacebo-7.387.0 --prefix="$media_prefix" --libdir=lib --wrap-mode=nodownload --buildtype=debugoptimized \
  -Dauto_features=disabled -Ddefault_library=shared -Dopengl=enabled -Dgl-proc-addr=enabled -Dlcms=enabled \
  -Dvulkan=disabled -Dshaderc=disabled -Dglslang=disabled -Ddemos=false -Dtests=false
meson compile -C build-media/placebo -j "${FF_BUILD_JOBS:-3}"
meson install -C build-media/placebo
meson setup --wipe build-media/mpv vendor/mpv-0.42.0 --prefix="$media_prefix" --libdir=lib --wrap-mode=nodownload --buildtype=debugoptimized \
  -Dauto_features=disabled -Dcplayer=false -Dlibmpv=true -Dgpl=false -Dbuild-date=false \
  -Dgl=enabled -Dplain-gl=enabled -Degl=enabled -Ddrm=enabled -Dvaapi=enabled -Dvaapi-drm=enabled \
  -Dwayland=enabled -Dvaapi-wayland=enabled \
  -Dpulse=enabled -Dlcms2=enabled -Dlibcurl=enabled -Diconv=enabled -Djpeg=enabled -Dzlib=enabled -Dvector=enabled \
  -Dlua=disabled -Djavascript=disabled \
  -Dlibarchive=disabled -Dlibbluray=disabled -Dlibavdevice=disabled -Ddvdnav=disabled -Ddvda=disabled -Dcplugins=disabled -Dvulkan=disabled
meson compile -C build-media/mpv -j "${FF_BUILD_JOBS:-3}"
meson install -C build-media/mpv
