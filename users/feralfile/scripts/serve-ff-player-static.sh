#!/bin/bash
# Serves the ff-player static tree baked at /opt/feral/ff-player (FFOS copies ff-player build output there).
# Default player navigation uses this loopback URL when ff1-config omits webapp_url.
# Requires darkhttpd on PATH unless FF_PLAYER_HTTP_SERVER is set to an alternate exec line.
set -euo pipefail

readonly FF_PLAYER_ROOT="${FF_PLAYER_STATIC_ROOT:-/opt/feral/ff-player}"
readonly PORT="${FF_PLAYER_STATIC_PORT:-8080}"

if [[ ! -d "${FF_PLAYER_ROOT}" ]]; then
	echo "serve-ff-player-static: skip, ${FF_PLAYER_ROOT} missing" >&2
	exit 0
fi

if [[ -n "${FF_PLAYER_HTTP_SERVER:-}" ]]; then
	# shellcheck disable=SC2086
	exec ${FF_PLAYER_HTTP_SERVER}
fi

if command -v darkhttpd >/dev/null 2>&1; then
	exec darkhttpd "${FF_PLAYER_ROOT}" --port "${PORT}" --addr 127.0.0.1
fi

# Directory exists (systemd condition) but no server binary: do not fail kiosk boot; fix the FFOS image (add darkhttpd) or set FF_PLAYER_HTTP_SERVER.
echo "serve-ff-player-static: darkhttpd not found; local webapp_url will not be served until fixed" >&2
exit 0
