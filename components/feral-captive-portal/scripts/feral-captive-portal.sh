#!/usr/bin/env bash
# Main entry: start hotspot (SSID = hostname), run captive portal HTTP server.
set -euo pipefail

INSTALL_ROOT="${CAPTIVE_PORTAL_INSTALL_ROOT:-/opt/feral/captive-portal}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -d "${SCRIPT_DIR}" && -f "${SCRIPT_DIR}/wifi-manager.sh" ]]; then
	INSTALL_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
fi

STATE_DIR="${CAPTIVE_PORTAL_STATE_DIR:-/home/feralfile/.state/captive-portal}"
LOG_TAG="feral-captive-portal"

mkdir -p "${STATE_DIR}"

if [[ -f /home/feralfile/.config/feral-captive-portal.env ]]; then
	# shellcheck disable=SC1091
	source /home/feralfile/.config/feral-captive-portal.env
fi

CONN_NAME="${CAPTIVE_PORTAL_CONN_NAME:-FeralCaptivePortal}"
PORT="${CAPTIVE_PORTAL_PORT:-8090}"
STOP_ON_SUCCESS="${CAPTIVE_PORTAL_STOP_ON_SUCCESS:-1}"
SUCCESS_FILE="${STATE_DIR}/connect-succeeded"
PREVIOUS_WIFI_FILE="${STATE_DIR}/previous-wifi-connection"
PID_FILE="${STATE_DIR}/portal.pid"

export CAPTIVE_PORTAL_INSTALL_ROOT="${INSTALL_ROOT}"
export CAPTIVE_PORTAL_CONN_NAME="${CONN_NAME}"
export CAPTIVE_PORTAL_PORT="${PORT}"

WIFI_IFACE="$("${INSTALL_ROOT}/scripts/detect-wifi-iface.sh")"
export CAPTIVE_PORTAL_WIFI_IFACE="${WIFI_IFACE}"

HOSTNAME="$(tr -d '[:space:]' < /etc/hostname)"
if [[ -z "${HOSTNAME}" ]]; then
	HOSTNAME="FF1"
fi
HOTSPOT_SSID="${CAPTIVE_PORTAL_HOTSPOT_SSID:-${HOSTNAME}}"
HOTSPOT_PASSWORD="${CAPTIVE_PORTAL_HOTSPOT_PASSWORD:-feralfile-setup}"

SERVER_PID=""
HOTSPOT_STARTED="false"

log() {
	printf '[%s] %s\n' "${LOG_TAG}" "$*" >&2
}

require_binary() {
	if ! command -v "$1" >/dev/null 2>&1; then
		log "required binary not found: $1"
		exit 1
	fi
}

hotspot_active() {
	nmcli -t -f NAME,DEVICE connection show --active 2>/dev/null \
		| grep -q "^${CONN_NAME}:${WIFI_IFACE}$"
}

stop_hotspot() {
	if hotspot_active; then
		log "stopping hotspot profile ${CONN_NAME}"
		nmcli connection down "${CONN_NAME}" >/dev/null 2>&1 || true
	fi
	rm -f "${STATE_DIR}/hotspot-active"
}

restore_saved_wifi() {
	local conn=""
	if [[ -f "${PREVIOUS_WIFI_FILE}" ]]; then
		IFS= read -r conn < "${PREVIOUS_WIFI_FILE}" || true
	fi
	if [[ -n "${conn}" ]]; then
		log "restoring saved Wi-Fi connection ${conn}"
		if nmcli connection up "${conn}" ifname "${WIFI_IFACE}" >/dev/null 2>&1; then
			return 0
		fi
		log "saved connection ${conn} did not come up; trying autoconnect"
	fi
	nmcli device connect "${WIFI_IFACE}" >/dev/null 2>&1 || true
}

ensure_hotspot_profile() {
	# WPA2-PSK AP: key-mgmt "none" maps to unsupported WEP on this driver stack.
	if nmcli connection show "${CONN_NAME}" >/dev/null 2>&1; then
		nmcli connection delete "${CONN_NAME}" >/dev/null 2>&1 || true
	fi

	log "creating hotspot profile SSID=${HOTSPOT_SSID}"
	nmcli connection add type wifi ifname "${WIFI_IFACE}" con-name "${CONN_NAME}" \
		autoconnect no ssid "${HOTSPOT_SSID}" \
		802-11-wireless.mode ap \
		802-11-wireless-security.key-mgmt wpa-psk \
		802-11-wireless-security.psk "${HOTSPOT_PASSWORD}" \
		ipv4.method shared ipv6.method ignore >/dev/null
}

start_hotspot() {
	ensure_hotspot_profile

	if hotspot_active; then
		log "hotspot already active on ${WIFI_IFACE}"
		HOTSPOT_STARTED="true"
		return 0
	fi

	# Drop any STA connection on the same radio before starting AP mode.
	local active_sta
	active_sta="$(nmcli -t -f NAME,TYPE,DEVICE connection show --active \
		| awk -F: -v dev="${WIFI_IFACE}" '$3 == dev && $2 == "802-11-wireless" && $1 != "'"${CONN_NAME}"'" { print $1; exit }')"
	if [[ -n "${active_sta}" ]]; then
		log "saving previous Wi-Fi connection ${active_sta}"
		printf '%s\n' "${active_sta}" >"${PREVIOUS_WIFI_FILE}"
		log "bringing down active Wi-Fi connection ${active_sta} before hotspot"
		nmcli connection down "${active_sta}" >/dev/null 2>&1 || true
	fi

	log "starting hotspot ${HOTSPOT_SSID} on ${WIFI_IFACE}"
	if ! nmcli connection up "${CONN_NAME}" ifname "${WIFI_IFACE}" >/dev/null 2>&1; then
		log "failed to activate hotspot; is dnsmasq installed?"
		exit 1
	fi

	date -Iseconds >"${STATE_DIR}/hotspot-active"
	HOTSPOT_STARTED="true"
	log "hotspot is active; portal SSID=${HOTSPOT_SSID}"
	setup_port_redirect
}

setup_port_redirect() {
	# Phones probe captive portals on port 80. Redirect to the unprivileged HTTP port.
	if [[ "${PORT}" == "80" ]]; then
		return 0
	fi
	if command -v iptables >/dev/null 2>&1; then
		if ! sudo iptables -t nat -C PREROUTING -i "${WIFI_IFACE}" -p tcp --dport 80 \
			-j REDIRECT --to-ports "${PORT}" >/dev/null 2>&1; then
			sudo iptables -t nat -A PREROUTING -i "${WIFI_IFACE}" -p tcp --dport 80 \
				-j REDIRECT --to-ports "${PORT}" || log "failed to install port 80 redirect"
		fi
	fi
}

teardown_port_redirect() {
	if [[ "${PORT}" == "80" ]]; then
		return 0
	fi
	if command -v iptables >/dev/null 2>&1; then
		sudo iptables -t nat -D PREROUTING -i "${WIFI_IFACE}" -p tcp --dport 80 \
			-j REDIRECT --to-ports "${PORT}" >/dev/null 2>&1 || true
	fi
}

start_portal_server() {
	require_binary python3
	log "starting portal HTTP server on 0.0.0.0:${PORT}"
	python3 "${INSTALL_ROOT}/server/portal_server.py" &
	SERVER_PID=$!
	printf '%s\n' "${SERVER_PID}" >"${PID_FILE}"
}

wait_for_server() {
	local deadline=$((SECONDS + 15))
	while (( SECONDS < deadline )); do
		if ! kill -0 "${SERVER_PID}" >/dev/null 2>&1; then
			log "portal server exited during startup"
			return 1
		fi
		if python3 - "${PORT}" <<'PY'
import socket
import sys

port = int(sys.argv[1])
sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock.settimeout(0.5)
try:
    sock.connect(("127.0.0.1", port))
except OSError:
    raise SystemExit(1)
finally:
    sock.close()
raise SystemExit(0)
PY
		then
			return 0
		fi
		sleep 0.5
	done
	log "timed out waiting for portal server on port ${PORT}"
	return 1
}

notify_ready() {
	if command -v systemd-notify >/dev/null 2>&1; then
		systemd-notify --ready --status="captive portal ready ssid=${HOTSPOT_SSID} port=${PORT}"
	fi
}

cleanup() {
	if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" >/dev/null 2>&1; then
		kill "${SERVER_PID}" >/dev/null 2>&1 || true
		wait "${SERVER_PID}" >/dev/null 2>&1 || true
	fi
	rm -f "${PID_FILE}"

	if hotspot_active; then
		stop_hotspot
	fi
	teardown_port_redirect

	# Successful provisioning keeps the newly joined network. Any other exit restores Wi-Fi.
	if [[ ! -f "${SUCCESS_FILE}" ]]; then
		restore_saved_wifi
	fi

	rm -f "${SUCCESS_FILE}" "${PREVIOUS_WIFI_FILE}"
}

trap cleanup EXIT INT TERM

require_binary nmcli
require_binary python3

if ! command -v dnsmasq >/dev/null 2>&1; then
	log "dnsmasq is required for NetworkManager shared hotspot (pacman -Sy dnsmasq)"
	exit 1
fi

start_hotspot
start_portal_server
wait_for_server
notify_ready

wait "${SERVER_PID}"
