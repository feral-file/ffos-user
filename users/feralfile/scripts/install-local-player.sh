#!/bin/bash
set -euo pipefail

SOURCE_PATH="${1:-}"
PLAYER_ROOT="/home/feralfile/.local/share/ff-player/current"
PLAYER_URL="${PLAYER_URL:-http://127.0.0.1:8314}"
OVERRIDE_PATH="/home/feralfile/.config/webapp-url"

if [[ -z "$SOURCE_PATH" ]]; then
  echo "usage: $0 <ff-player-export-dir-or-tgz>" >&2
  exit 1
fi

mkdir -p "$(dirname "$PLAYER_ROOT")"
rm -rf "$PLAYER_ROOT"
mkdir -p "$PLAYER_ROOT"

if [[ -d "$SOURCE_PATH" ]]; then
  cp -R "$SOURCE_PATH"/. "$PLAYER_ROOT"/
elif [[ -f "$SOURCE_PATH" ]]; then
  tar -xzf "$SOURCE_PATH" -C "$PLAYER_ROOT"
else
  echo "source path not found: $SOURCE_PATH" >&2
  exit 1
fi

printf '%s\n' "$PLAYER_URL" > "$OVERRIDE_PATH"

systemctl --user daemon-reload
systemctl --user enable --now ff-player-local.service
systemctl --user restart ff-player-local.service
systemctl --user restart feral-setupd.service

echo "installed local ff-player bundle to $PLAYER_ROOT"
echo "webapp override set to $PLAYER_URL"
