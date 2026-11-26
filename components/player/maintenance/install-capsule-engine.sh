#!/usr/bin/env bash
# Install an operator-supplied DP-1 engine archive under its compressed SHA-256.
set -euo pipefail
[[ $# == 2 && $1 =~ ^[0-9a-f]{64}$ && $EUID == 0 ]] || {
  echo "Usage (as root): install-capsule-engine.sh SHA256 ARCHIVE.tar.zst" >&2
  exit 2
}
digest=$1
archive=$(realpath -- "$2")
store=/var/lib/ff-player-native/engines
install -d -m 0755 "$store"
exec 9<"$store"
flock 9
stage=$(mktemp -d "$store/.install.XXXXXXXX")
trap 'rm -rf -- "$stage"' EXIT
# Verify a private snapshot so the input cannot change between checking and unpacking.
cp -- "$archive" "$stage/archive"
(cd "$stage" && echo "$digest  archive" | sha256sum --check --status)
[[ ! -d "$store/$digest" ]] || exit 0
install -d -o nobody -g nobody -m 0755 "$stage/root"
(cd "$stage/root" && zstd -dc "$stage/archive" |
  setpriv --reuid=nobody --regid=nobody --clear-groups --no-new-privs \
    bsdtar -xf - --no-same-owner --no-same-permissions)
[[ -f "$stage/root/usr/lib/chromium/chromium" && -x "$stage/root/usr/lib/chromium/chromium" ]]
chown -hR root:root "$stage/root"
find "$stage/root" -type f -exec chmod a-s,go-w {} +
find "$stage/root" -type d -exec chmod a-s,go-w {} +
mv -T "$stage/root" "$store/$digest"
