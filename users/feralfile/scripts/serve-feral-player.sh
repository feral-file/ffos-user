#!/bin/bash
set -euo pipefail

readonly FF_PLAYER_ROOT="${FF_PLAYER_STATIC_ROOT:-/opt/feral/feral-player}"
readonly FF_PLAYER_PORT="${FF_PLAYER_STATIC_PORT:-8080}"
readonly FF_PLAYER_URL="http://127.0.0.1:${FF_PLAYER_PORT}/"
readonly FF_PLAYER_READY_TIMEOUT_SECONDS="${FF_PLAYER_READY_TIMEOUT_SECONDS:-30}"
readonly FF_PLAYER_READY_POLL_SECONDS="${FF_PLAYER_READY_POLL_SECONDS:-1}"
readonly FF_PLAYER_CONTRACT_FILE="${FF_PLAYER_ROOT}/ffos-player-contract.json"
readonly FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT="${FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT:-0}"
readonly FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT="${FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT:-1}"

require_binary() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "serve-feral-player: required binary not found: $1" >&2
		exit 1
	fi
}

# SoftAP onboarding narrates entirely through the player's setupDisplay CDP
# contract (soft-AP QR, join feedback, claim QR). controld deliberately degrades
# to narration-disabled when the bundle lacks it, so without this gate a bad
# bundle reaches READY and a fresh offline device sits with no displayed setup
# credentials. Default-required for exactly that reason (unlike the optional
# mint-pairing gate below); FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT=0 is the
# dev/legacy-bundle escape hatch. The checks mirror
# setupui.validateSetupDisplayContract — keep the two in sync.
validate_setup_display_contract() {
	if [[ "${FF_PLAYER_REQUIRE_SETUP_DISPLAY_CONTRACT}" != "1" ]]; then
		return 0
	fi

	if [[ ! -f "${FF_PLAYER_CONTRACT_FILE}" ]]; then
		echo "serve-feral-player: missing player contract manifest at ${FF_PLAYER_CONTRACT_FILE}" >&2
		exit 1
	fi

	require_binary python3
	if ! python3 - "${FF_PLAYER_CONTRACT_FILE}" <<'PY'
import json
import sys

path = sys.argv[1]
required_states = {"softap_qr", "joining", "join_failed", "updating", "claim_qr", "ready", "hidden"}

with open(path, encoding="utf-8") as fh:
    manifest = json.load(fh)

contract = manifest.get("contracts", {}).get("setupDisplay")
if not isinstance(contract, dict):
    raise SystemExit("missing contracts.setupDisplay")
if contract.get("version") != 1:
    raise SystemExit("contracts.setupDisplay.version must be 1")
if contract.get("requestKey") != "request":
    raise SystemExit('contracts.setupDisplay.requestKey must be "request"')
states = contract.get("states")
if not isinstance(states, list) or not required_states.issubset(set(states)):
    raise SystemExit("contracts.setupDisplay.states missing required states")
accepted = contract.get("acceptedResponse")
if not isinstance(accepted, dict) or accepted.get("ok") is not True:
    raise SystemExit("contracts.setupDisplay.acceptedResponse.ok must be true")
PY
	then
		echo "serve-feral-player: invalid setupDisplay player contract at ${FF_PLAYER_CONTRACT_FILE}" >&2
		exit 1
	fi
}

validate_player_contract() {
	if [[ "${FF_PLAYER_REQUIRE_MINT_PAIRING_CONTRACT}" != "1" ]]; then
		return 0
	fi

	if [[ ! -f "${FF_PLAYER_CONTRACT_FILE}" ]]; then
		echo "serve-feral-player: missing player contract manifest at ${FF_PLAYER_CONTRACT_FILE}" >&2
		exit 1
	fi

	require_binary python3
	if ! python3 - "${FF_PLAYER_CONTRACT_FILE}" <<'PY'
import json
import sys

path = sys.argv[1]
required_states = {"pairing_code", "request_received", "creating_token", "hidden"}

with open(path, encoding="utf-8") as fh:
    manifest = json.load(fh)

contract = manifest.get("contracts", {}).get("mintPairingDisplay")
if not isinstance(contract, dict):
    raise SystemExit("missing contracts.mintPairingDisplay")
if contract.get("version") != 1:
    raise SystemExit("contracts.mintPairingDisplay.version must be 1")
if contract.get("requestKey") != "request":
    raise SystemExit('contracts.mintPairingDisplay.requestKey must be "request"')
states = contract.get("states")
if not isinstance(states, list) or not required_states.issubset(set(states)):
    raise SystemExit("contracts.mintPairingDisplay.states missing required states")
accepted = contract.get("acceptedResponse")
if not isinstance(accepted, dict) or accepted.get("ok") is not True:
    raise SystemExit("contracts.mintPairingDisplay.acceptedResponse.ok must be true")
PY
	then
		echo "serve-feral-player: invalid player contract manifest at ${FF_PLAYER_CONTRACT_FILE}" >&2
		exit 1
	fi
}

start_server() {
	if [[ ! -d "${FF_PLAYER_ROOT}" ]]; then
		echo "serve-feral-player: missing static tree at ${FF_PLAYER_ROOT}" >&2
		exit 1
	fi

	validate_setup_display_contract
	validate_player_contract

	require_binary darkhttpd
	require_binary curl
	require_binary systemd-notify

	# darkhttpd emits no Cache-Control headers on its own, so Chromium
	# heuristically caches the route HTML and even negative (404) responses —
	# stale entries that outlive bundle swaps and reboots (#234). A single
	# global no-cache forces revalidation on every use (loopback 304s via
	# If-Modified-Since, effectively free) and darkhttpd applies custom
	# headers to error responses too, so a 404 captured mid-swap cannot
	# poison later loads of a live Chromium session — the one window the
	# kiosk-side bundle-fingerprint purge cannot close (it only runs between
	# Chromium instances). Per-path policy (immutable _next/static, no-cache
	# HTML) is not expressible with darkhttpd; global no-cache is the safe
	# degradation. Probe for --header support instead of assuming it: an
	# older binary must degrade to headerless serving (the fingerprint purge
	# still covers bundle changes), not die in a flag-error restart loop.
	# ${arr[@]+...} keeps the empty-array expansion safe under `set -u` on
	# bash < 4.4 (macOS test harness).
	# The no-arg usage dump MAY exit non-zero (1.17 measures 0; other
	# builds differ — assume neither). Under `set -e` an unguarded
	# `var=$(darkhttpd 2>&1)` aborts the script at this line — the
	# unit would die before ever starting the server and Restart=on-failure
	# would loop it. `|| true` is what keeps the probe non-fatal; do not
	# remove it. The match is anchored so a hypothetical --no-header (or a
	# mention in prose) cannot false-positive into passing an unknown flag,
	# which would exit darkhttpd and restart-loop the unit.
	local darkhttpd_usage
	darkhttpd_usage=$(darkhttpd 2>&1 || true)
	local -a cache_header_args=()
	if grep -Eq -- '(^|[^-[:alnum:]])--header([[:space:]]|$)' <<<"$darkhttpd_usage"; then
		cache_header_args=(--header 'Cache-Control: no-cache')
	else
		# Degrading silently would make "is the #234 header mitigation
		# active?" unanswerable from the journal; the fingerprint purge in
		# start-kiosk.sh is then the only remaining defense.
		echo "serve-feral-player: darkhttpd has no --header support; serving without Cache-Control (see #234)" >&2
	fi

	darkhttpd "${FF_PLAYER_ROOT}" --port "${FF_PLAYER_PORT}" --addr 127.0.0.1 \
		${cache_header_args[@]+"${cache_header_args[@]}"} &
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
