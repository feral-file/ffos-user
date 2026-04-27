#!/bin/bash
set -euo pipefail

readonly FF_PLAYER_ROOT="${FF_PLAYER_STATIC_ROOT:-/opt/feral/feral-player}"
readonly FF_PLAYER_PORT="${FF_PLAYER_STATIC_PORT:-8080}"
readonly FF_PLAYER_URL="http://127.0.0.1:${FF_PLAYER_PORT}/"
readonly FF_PLAYER_READY_TIMEOUT_SECONDS="${FF_PLAYER_READY_TIMEOUT_SECONDS:-30}"
readonly FF_PLAYER_READY_POLL_SECONDS="${FF_PLAYER_READY_POLL_SECONDS:-1}"

require_binary() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "serve-feral-player: required binary not found: $1" >&2
		exit 1
	fi
}

start_server() {
	if [[ ! -d "${FF_PLAYER_ROOT}" ]]; then
		echo "serve-feral-player: missing static tree at ${FF_PLAYER_ROOT}" >&2
		exit 1
	fi

	require_binary darkhttpd
	require_binary curl
	require_binary systemd-notify

	darkhttpd "${FF_PLAYER_ROOT}" --port "${FF_PLAYER_PORT}" --addr 127.0.0.1 &
	server_pid=$!
}

wait_for_ready() {
	local deadline
	deadline=$((SECONDS + FF_PLAYER_READY_TIMEOUT_SECONDS))

	while (( SECONDS < deadline )); do
		if curl -fsS --max-time 1 "${FF_PLAYER_URL}" >/dev/null 2>&1; then
			return 0
		fi

		if ! kill -0 "${server_pid}" >/dev/null 2>&1; then
			wait "${server_pid}" || true
			echo "serve-feral-player: HTTP server exited before becoming ready" >&2
			return 1
		fi

		sleep "${FF_PLAYER_READY_POLL_SECONDS}"
	done

	echo "serve-feral-player: timed out waiting for ${FF_PLAYER_URL}" >&2
	return 1
}

cleanup() {
	if [[ -n "${server_pid:-}" ]] && kill -0 "${server_pid}" >/dev/null 2>&1; then
		kill "${server_pid}" >/dev/null 2>&1 || true
		wait "${server_pid}" >/dev/null 2>&1 || true
	fi
}

trap cleanup EXIT INT TERM

start_server
wait_for_ready
systemd-notify --ready --status="feral-player static ready on ${FF_PLAYER_URL}"
wait "${server_pid}"
