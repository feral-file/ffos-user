#!/bin/sh
# Run from a complete Linux source bundle with private media dependencies built.
# Neither phase installs a system package or starts a browser. After the updated
# phase, stage/install it separately and run the device suite documented in ENGINE.md.
set -eu
test "$(uname -s)" = Linux
test "$#" = 2 || { echo 'usage: engine-update-rehearsal.sh BASELINE-CEF UPDATED-CEF' >&2; exit 2; }
rehearsal_old=$(realpath "$1")
rehearsal_new=$(realpath "$2")
rehearsal_root=$(pwd)
rehearsal_media=${FF_MEDIA_PREFIX:-"$rehearsal_root/build-media/prefix"}
rehearsal_build="$rehearsal_root/build-engine-update"
rehearsal_logs="$rehearsal_root/build-engine-update-evidence"
mkdir -p "$rehearsal_logs"
export PKG_CONFIG_PATH="$rehearsal_media/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
export LD_LIBRARY_PATH="$rehearsal_media/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
for rehearsal_phase in baseline updated; do
  if test "$rehearsal_phase" = baseline; then rehearsal_cef=$rehearsal_old; else rehearsal_cef=$rehearsal_new; fi
  python3 - "$rehearsal_phase" "$rehearsal_cef" <<'PY'
import json, re, sys
from pathlib import Path
lock=json.loads(Path('maintenance/engine-update.json').read_text())
header=(Path(sys.argv[2])/'include/cef_version.h').read_text()
version=re.search(r'^#define CEF_VERSION "([^"]+)"',header,re.M).group(1)
assert version==lock[sys.argv[1]]['version'],(version,lock[sys.argv[1]]['version'])
print(sys.argv[1],version,flush=True)
PY
  cmake -S "$rehearsal_root" -B "$rehearsal_build" -G Ninja \
    -DFF_WITH_CEF=ON -DCEF_ROOT="$rehearsal_cef" -DFF_MEDIA_PREFIX="$rehearsal_media" \
    -DCMAKE_BUILD_TYPE=RelWithDebInfo -DCMAKE_INSTALL_PREFIX=/usr \
    > "$rehearsal_logs/$rehearsal_phase-configure.log" 2>&1
  cmake --build "$rehearsal_build" -j "${FF_BUILD_JOBS:-3}" > "$rehearsal_logs/$rehearsal_phase-build.log" 2>&1
  ctest --test-dir "$rehearsal_build" --output-on-failure > "$rehearsal_logs/$rehearsal_phase-ctest.log" 2>&1
  cp "$rehearsal_cef/include/cef_version.h" "$rehearsal_logs/$rehearsal_phase-cef-version.h"
  sha256sum "$rehearsal_build/ff-player-native" > "$rehearsal_logs/$rehearsal_phase-host.sha256"
done
echo 'Both engine builds and core suites passed; updated-engine device qualification is still required.'
