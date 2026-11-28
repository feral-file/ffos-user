#!/bin/bash
set -euo pipefail
base=/home/feralfile/ff-native-office-benchmark
mkdir -p "$base"
cd "$base"
curl --fail --location --retry 2 --output chrome.zip https://storage.googleapis.com/chrome-for-testing-public/156.0.8078.80/linux64/chrome-linux64.zip
printf '%s  %s\n' b711c720f774be355197f32edf37e0651418ba036d92446ee2da19ac017540a7 chrome.zip | sha256sum --check
curl --fail --location --retry 2 --output cef.tar.bz2 'https://cef-builds.spotifycdn.com/cef_binary_156.0.15+gcf671c0+chromium-156.0.8078.80_linux64_minimal.tar.bz2'
printf '%s  %s\n' 6873f975bb047316680ea1839980ed5bd4c33c8cd4ce45761c94211856605388 cef.tar.bz2 | sha256sum --check
python3 -c 'import zipfile;zipfile.ZipFile("chrome.zip").extractall()'
chmod +x chrome-linux64/chrome chrome-linux64/chrome_crashpad_handler chrome-linux64/chrome_sandbox
mkdir -p cef
tar -xjf cef.tar.bz2 -C cef --strip-components=1
export LD_LIBRARY_PATH=/home/feralfile/ff-native-debug/build-media/prefix/lib
export PKG_CONFIG_PATH=/home/feralfile/ff-native-debug/build-media/prefix/lib/pkgconfig
cmake -S /home/feralfile/ff-native-debug -B build -G Ninja -DCMAKE_BUILD_TYPE=Release -DFF_WITH_CEF=ON -DCEF_ROOT="$base/cef" -DCMAKE_INSTALL_PREFIX=/usr
cmake --build build -j2
ctest --test-dir build --output-on-failure
DESTDIR="$base/stage" cmake --install build
sudo chown root:root "$base/stage/usr/lib/ff-player-native/chrome-sandbox" "$base/chrome-linux64/chrome_sandbox"
sudo chmod 4755 "$base/stage/usr/lib/ff-player-native/chrome-sandbox" "$base/chrome-linux64/chrome_sandbox"
