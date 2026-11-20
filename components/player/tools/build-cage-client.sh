#!/bin/sh
set -eu
revision=9f93103442ac6adb6e09f4f95440030f38b48866
work="$PWD/build-cage-client"
# Extract into a fresh tree so repeated builds use the upstream Cargo.lock.
mkdir -p "$work"
work=$(mktemp -d "$work/source.XXXXXX")
export CARGO_TARGET_DIR="$PWD/build-cage-client/target"
curl -fsSL --retry 3 "https://codeload.github.com/mahkoh/wl-proxy/tar.gz/$revision" -o "$work/source.tar.gz"
echo "92966776f2f0df2c0fcbcd0da9dd8a13caefc0e53c10dccbb1c12c9b06bbe6bb  $work/source.tar.gz" | sha256sum -c -
tar -xf "$work/source.tar.gz" --strip-components=1 -C "$work"
mkdir -p "$work/apps/ff-cage-client"
cp -r cage-client/. "$work/apps/ff-cage-client/"
cat >> "$work/Cargo.lock" <<'LOCK'

[[package]]
name = "ff-cage-client"
version = "0.1.0"
dependencies = ["wl-proxy"]
LOCK
cargo build --locked --release --manifest-path "$work/Cargo.toml" -p ff-cage-client
mkdir -p "$PWD/build-cage-client/licenses"
for license in LICENSE-MIT LICENSE-APACHE; do
  install -m644 "$work/wl-proxy/$license" "$PWD/build-cage-client/licenses/$license"
done
