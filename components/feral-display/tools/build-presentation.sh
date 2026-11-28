#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
prefix="$root/build-presentation/prefix"
export PKG_CONFIG_PATH="$prefix/lib/pkgconfig:$prefix/share/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
export PATH="$prefix/bin:$PATH"
export LD_LIBRARY_PATH="$prefix/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
mkdir -p "$prefix"
# CI's pinned Ubuntu image may have older build SDKs than FFOS. These SDKs
# stay in the build prefix; the FFOS package requires the corresponding system
# ABI. Only our focused headless wlroots build ships with the player.
build() {
 name=$1; archive=$2; shift 2
 if [ ! -d "$root/vendor/$name" ]; then tar -xf "$root/vendor/$archive" -C "$root/vendor"; fi
 meson setup "$root/build-presentation/$name" "$root/vendor/$name" \
   --prefix "$prefix" --libdir lib --buildtype release --wrap-mode nodownload "$@"
 meson compile -C "$root/build-presentation/$name" -j "${FF_BUILD_JOBS:-3}"
 meson install -C "$root/build-presentation/$name"
}
if ! pkg-config --atleast-version=2.4.129 libdrm; then
 build libdrm-2.4.129 libdrm-2.4.129.tar.xz -Dauto_features=disabled -Dtests=false
fi
if ! pkg-config --atleast-version=1.24.0 wayland-server; then
 build wayland-1.24.0 wayland-1.24.0.tar.xz -Ddocumentation=false -Dtests=false -Ddtd_validation=false
fi
if ! pkg-config --atleast-version=1.47 wayland-protocols; then
 build wayland-protocols-1.47 wayland-protocols-1.47.tar.xz -Dtests=false
fi
build wlroots-0.20.0 wlroots-0.20.0.tar.gz \
 -Dbackends=[] -Dallocators=gbm -Drenderers=gles2 -Dsession=disabled \
 -Dxwayland=disabled -Dxcb-errors=disabled -Dexamples=false -Dcolor-management=disabled -Dlibliftoff=disabled
