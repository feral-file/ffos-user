#!/usr/bin/env bash
# nmcli helpers for captive portal provisioning.
# Mirrors feral-setupd wifi_utils.rs behavior without touching setupd.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

readonly WIFI_IFACE="${CAPTIVE_PORTAL_WIFI_IFACE:-$("${SCRIPT_DIR}/detect-wifi-iface.sh")}"
readonly MAX_NETWORKS="${CAPTIVE_PORTAL_MAX_NETWORKS:-20}"

log() {
	printf '[wifi-manager] %s\n' "$*" >&2
}

delete_connection() {
	local ssid="$1"
	nmcli connection delete "${ssid}" >/dev/null 2>&1 || true
}

list_networks() {
	local force="${1:-yes}"
	local -a args=( -t -f SSID,SIGNAL,SECURITY device wifi list )
	if [[ "${force}" == "yes" ]]; then
		args+=( --rescan yes )
	fi

	run_scan() {
		nmcli "${args[@]}" 2>/dev/null || true
	}

	mapfile -t lines < <(run_scan)

	# Single-radio hardware often cannot rescan while AP mode is active.
	if [[ "${#lines[@]}" -eq 0 ]] && nmcli -t -f NAME,DEVICE connection show --active \
		| grep -q "^${CAPTIVE_PORTAL_CONN_NAME:-FeralCaptivePortal}:${WIFI_IFACE}$"; then
		log "scan empty while hotspot active; dropping AP briefly to rescan"
		nmcli connection down "${CAPTIVE_PORTAL_CONN_NAME:-FeralCaptivePortal}" >/dev/null 2>&1 || true
		sleep 2
		mapfile -t lines < <(run_scan)
		if ! nmcli connection up "${CAPTIVE_PORTAL_CONN_NAME:-FeralCaptivePortal}" ifname "${WIFI_IFACE}" >/dev/null 2>&1; then
			log "failed to restore hotspot after rescan"
			printf '[]\n'
			return 1
		fi
	fi

	if [[ "${#lines[@]}" -eq 0 ]]; then
		printf '[]\n'
		return 0
	fi

	python3 - "${MAX_NETWORKS}" "${lines[@]}" <<'PY'
import json
import sys

max_items = int(sys.argv[1])
rows = sys.argv[2:]
seen = set()
networks = []

for row in rows:
    parts = row.split(":", 2)
    if not parts:
        continue
    ssid = parts[0].strip()
    if not ssid or ssid in seen:
        continue
    seen.add(ssid)
    signal = int(parts[1]) if len(parts) > 1 and parts[1].isdigit() else 0
    security = parts[2].strip() if len(parts) > 2 else ""
    networks.append({
        "ssid": ssid,
        "signal": signal,
        "security": security,
        "secured": security not in ("", "--"),
    })
    if len(networks) >= max_items:
        break

networks.sort(key=lambda item: item["signal"], reverse=True)
print(json.dumps(networks))
PY
}

connection_status() {
	local active_ssid
	active_ssid="$(nmcli -t -f NAME,TYPE connection show --active | awk -F: '$2 == "802-11-wireless" { print $1; exit }')"
	local hotspot_active="false"
	if nmcli -t -f NAME,DEVICE connection show --active | grep -q "^${CAPTIVE_PORTAL_CONN_NAME:-FeralCaptivePortal}:${WIFI_IFACE}$"; then
		hotspot_active="true"
	fi

	python3 - "${active_ssid:-}" "${hotspot_active}" "${WIFI_IFACE}" <<'PY'
import json
import sys

active = sys.argv[1]
hotspot = sys.argv[2] == "true"
iface = sys.argv[3]
print(json.dumps({
    "wifi_iface": iface,
    "hotspot_active": hotspot,
    "connected_ssid": active,
    "online": bool(active) and not hotspot,
}))
PY
}

connect_network() {
	local ssid="$1"
	local password="$2"

	if [[ -z "${ssid}" ]]; then
		echo '{"ok":false,"error":"ssid is required"}'
		return 1
	fi

	log "connecting to SSID=${ssid}"
	delete_connection "${ssid}"

	local -a args=( device wifi connect "${ssid}" )
	if [[ -n "${password}" ]]; then
		args+=( password "${password}" )
	fi

	local stderr
	if stderr="$(nmcli "${args[@]}" 2>&1 >/dev/null)"; then
		python3 - "${ssid}" <<'PY'
import json
import sys
print(json.dumps({"ok": True, "ssid": sys.argv[1]}))
PY
		return 0
	fi

	python3 - "${stderr}" <<'PY'
import json
import sys

message = sys.argv[1].strip() or "connection failed"
lower = message.lower()
if "secrets were required" in lower or "password" in lower:
    code = "wrong_password"
elif "no network with ssid" in lower:
    code = "network_not_found"
else:
    code = "connection_failed"
print(json.dumps({"ok": False, "error": message, "code": code}))
PY
	return 1
}

case "${1:-}" in
	list)
		list_networks "${2:-yes}"
		;;
	status)
		export WIFI_IFACE="${WIFI_IFACE}"
		connection_status
		;;
	connect)
		connect_network "${2:-}" "${3:-}"
		;;
	*)
		echo "usage: $0 {list|status|connect <ssid> [password]}" >&2
		exit 2
		;;
esac
