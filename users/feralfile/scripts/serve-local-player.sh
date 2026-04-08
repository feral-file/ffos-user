#!/bin/bash
set -euo pipefail

PLAYER_ROOT="${PLAYER_ROOT:-/home/feralfile/.local/share/ff-player/current}"
PLAYER_HOST="${PLAYER_HOST:-127.0.0.1}"
PLAYER_PORT="${PLAYER_PORT:-8314}"

if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is required to serve the local ff-player bundle" >&2
  exit 1
fi

if [[ ! -d "$PLAYER_ROOT" ]]; then
  echo "local ff-player bundle not found at $PLAYER_ROOT" >&2
  exit 1
fi

exec python3 -m http.server "$PLAYER_PORT" \
  --bind "$PLAYER_HOST" \
  --directory "$PLAYER_ROOT"
