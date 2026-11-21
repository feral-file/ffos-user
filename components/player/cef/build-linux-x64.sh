#!/usr/bin/env bash
set -euo pipefail

recipe=$(cd -- "$(dirname -- "$0")" && pwd)
work=${1:?Pass an absolute build directory on the Linux CI runner}
case "$work" in /*) ;; *) echo "Build directory must be absolute" >&2; exit 2;; esac
mkdir -p "$work"
test "$(uname -m)" = x86_64
available_kib=$(df -Pk "$work" | awk 'NR == 2 { print $4 }')
if (( available_kib < 150 * 1024 * 1024 )); then
    df -h "$work" >&2
    echo "CEF needs at least 150 GiB free. Set CEF_BUILD_RUNNER to a larger Ubuntu x64 runner." >&2
    exit 1
fi

cef_revision=426920c33ef22551e0b8f48cf343d623ac25642d
depot_revision=1b0972d3d5e5b1e78055147cb3576361e274b9ed
version=156.0.8+g426920c+chromium-156.0.8078.48
curl --fail --silent --show-error --location --retry 3 \
    "https://raw.githubusercontent.com/chromiumembedded/cef/$cef_revision/tools/automate/automate-git.py" \
    -o "$work/automate-git.py"

git init "$work/depot_tools"
git -C "$work/depot_tools" fetch --depth=1 https://chromium.googlesource.com/chromium/tools/depot_tools.git "$depot_revision"
git -C "$work/depot_tools" checkout --detach FETCH_HEAD
export DEPOT_TOOLS_UPDATE=0
"$work/depot_tools/ensure_bootstrap"

# CEF's generator consumes GN_DEFINES as shell assignments.
GN_DEFINES=$(sed -E 's/ = /=/; s/"//g' "$recipe/linux-x64.gn" | paste -sd ' ' -)
export GN_DEFINES
automate=(python3 "$work/automate-git.py" --download-dir="$work" --branch=8078
    --checkout="$cef_revision" --no-depot-tools-update --no-chromium-history
    --x64-build --no-debug-build)
"${automate[@]}" --no-build --no-distrib
src="$work/chromium/src"
sudo "$src/build/install-build-deps.sh" --no-prompt --no-chromeos-fonts \
    --no-arm --no-nacl --no-backwards-compatible
build_target=cefsimple
if [[ -n "${FF_CEF_BUILD_JOBS:-}" ]]; then
    [[ "$FF_CEF_BUILD_JOBS" =~ ^[1-9][0-9]*$ ]]
    # The pinned automate-git passes this string as arguments to autoninja.
    build_target="-j $FF_CEF_BUILD_JOBS $build_target"
fi
"${automate[@]}" --no-update --force-build --build-target="$build_target" \
    --minimal-distrib-only --no-distrib-archive --no-distrib-docs --no-distrib-symbols \
    --distrib-subdir=ff-cef-sdk

sdk="$src/cef/binary_distrib/ff-cef-sdk_minimal"
grep -Fx "#define CEF_VERSION \"$version\"" "$sdk/include/cef_version.h"
args="$src/out/Release_GN_x64/args.gn"
# CEF writes assignments without spaces around the equals sign.
while IFS= read -r setting; do
    grep -Fx "$setting" "$args"
done < <(sed 's/ = /=/' "$recipe/linux-x64.gn")
test -s "$sdk/Release/libcef.so"
test -x "$sdk/Release/chrome-sandbox"
test -s "$sdk/Resources/icudtl.dat"

output="$work/artifact"
mkdir -p "$output"
tar -C "$(dirname "$sdk")" -cf - "$(basename "$sdk")" | xz -T2 > "$output/cef.tar.xz"
cp "$args" "$output/args.gn"
python3 - "$output" "$version" "$cef_revision" "$depot_revision" <<'PY'
import hashlib
import json
import sys
from pathlib import Path

directory, version, cef_revision, depot_revision = sys.argv[1:]
directory = Path(directory)
with (directory / "cef.tar.xz").open("rb") as archive:
    digest = hashlib.file_digest(archive, "sha256").hexdigest()
(directory / "cef.json").write_text(json.dumps({
    "version": version,
    "platform": "linux64",
    "file": "cef.tar.xz",
    "sha256": digest,
    "cefRevision": cef_revision,
    "depotToolsRevision": depot_revision,
}, indent=2) + "\n")
PY
