#!/usr/bin/env bash
# Shared helpers for captive portal lifecycle scripts.
set -euo pipefail

captive_portal_state_dir() {
	printf '%s\n' "${CAPTIVE_PORTAL_STATE_DIR:-/home/feralfile/.state/captive-portal}"
}

captive_portal_conn_name() {
	printf '%s\n' "${CAPTIVE_PORTAL_CONN_NAME:-FeralCaptivePortal}"
}

captive_portal_install_root() {
	printf '%s\n' "${CAPTIVE_PORTAL_INSTALL_ROOT:-/opt/feral/captive-portal}"
}

captive_portal_wifi_iface() {
	if [[ -n "${CAPTIVE_PORTAL_WIFI_IFACE:-}" ]]; then
		printf '%s\n' "${CAPTIVE_PORTAL_WIFI_IFACE}"
		return 0
	fi
	"$(captive_portal_install_root)/scripts/detect-wifi-iface.sh"
}

captive_portal_log() {
	printf '[feral-captive-portal] %s\n' "$*" >&2
}

captive_portal_hotspot_active() {
	local conn iface
	conn="$(captive_portal_conn_name)"
	iface="$(captive_portal_wifi_iface)"
	nmcli -t -f NAME,DEVICE connection show --active 2>/dev/null \
		| grep -q "^${conn}:${iface}$"
}

captive_portal_stop_hotspot() {
	local conn
	conn="$(captive_portal_conn_name)"
	if captive_portal_hotspot_active; then
		captive_portal_log "stopping hotspot profile ${conn}"
		nmcli connection down "${conn}" >/dev/null 2>&1 || true
	fi
}

captive_portal_stop_server() {
	local state_dir pid_file pid
	state_dir="$(captive_portal_state_dir)"
	pid_file="${state_dir}/portal.pid"

	if [[ -f "${pid_file}" ]]; then
		pid="$(tr -d '[:space:]' < "${pid_file}")"
		if [[ -n "${pid}" ]] && kill -0 "${pid}" >/dev/null 2>&1; then
			captive_portal_log "stopping portal server pid=${pid}"
			kill "${pid}" >/dev/null 2>&1 || true
			wait "${pid}" >/dev/null 2>&1 || true
		fi
		rm -f "${pid_file}"
	fi

	if command -v systemctl >/dev/null 2>&1; then
		systemctl --user stop feral-captive-portal.service >/dev/null 2>&1 || true
	fi
}

captive_portal_restore_wifi() {
	local state_dir saved iface conn
	state_dir="$(captive_portal_state_dir)"
	saved="${state_dir}/previous-wifi-connection"
	iface="$(captive_portal_wifi_iface)"

	captive_portal_stop_hotspot
	rm -f "${state_dir}/hotspot-active"

	if [[ -f "${saved}" ]]; then
		IFS= read -r conn < "${saved}" || true
		if [[ -n "${conn}" ]]; then
			captive_portal_log "restoring saved Wi-Fi connection ${conn}"
			if nmcli connection up "${conn}" ifname "${iface}" >/dev/null 2>&1; then
				return 0
			fi
			captive_portal_log "saved connection ${conn} did not come up; trying autoconnect"
		fi
	fi

	captive_portal_log "asking NetworkManager to autoconnect ${iface}"
	nmcli device connect "${iface}" >/dev/null 2>&1 || true
}

captive_portal_clear_session_state() {
	local state_dir
	state_dir="$(captive_portal_state_dir)"
	rm -f \
		"${state_dir}/hotspot-active" \
		"${state_dir}/portal.pid" \
		"${state_dir}/connect-succeeded" \
		"${state_dir}/connect-job.json"
}
