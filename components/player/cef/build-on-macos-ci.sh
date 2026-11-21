#!/usr/bin/env bash
set -euo pipefail

recipe=$(cd -- "$(dirname -- "$0")" && pwd)
output=${1:?Pass the SDK artifact output directory}
uname -m
df -h "$RUNNER_TEMP"
docker info --format 'Docker guest: {{.OSType}}/{{.Architecture}}, {{.NCPU}} CPUs, {{.MemTotal}} bytes RAM'

mkdir -p "$output"
output=$(cd -- "$output" && pwd -P)
container=$(docker create --platform linux/amd64 \
    --mount "type=bind,source=$recipe,target=/recipe,readonly" \
    --mount "type=bind,source=$output,target=/artifact" \
    --mount type=volume,destination=/cef-work \
    ubuntu:24.04 bash -euc '
        memory_kib=$(awk "/^MemTotal:/ { print \$2 }" /proc/meminfo)
        if (( memory_kib < 8 * 1024 * 1024 )); then
            echo "Allocate at least 8 GiB of RAM to Docker for CEF." >&2
            exit 1
        fi
        df -h /cef-work
        export DEBIAN_FRONTEND=noninteractive
        apt-get update
        apt-get install -y --no-install-recommends ca-certificates curl file git lsb-release python3 sudo xz-utils
        # Leave memory for the linker on the 16 GB Mac runner.
        FF_CEF_BUILD_JOBS=4 bash /recipe/build-linux-x64.sh /cef-work
        cp /cef-work/artifact/* /artifact/
    ')
trap 'docker rm -fv "$container" >/dev/null' EXIT
docker start --attach "$container"
test "$(docker inspect --format '{{.State.ExitCode}}' "$container")" = 0
test -s "$output/cef.tar.xz"
