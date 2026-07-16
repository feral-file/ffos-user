#!/usr/bin/env bash
# Resolve the Wi-Fi interface managed by NetworkManager.
set -euo pipefail

if [[ -n "${CAPTIVE_PORTAL_WIFI_IFACE:-}" ]]; then
	printf '%s\n' "${CAPTIVE_PORTAL_WIFI_IFACE}"
	exit 0
fi

iface="$(nmcli -t -f DEVICE,TYPE device status | awk -F: '$2 == "wifi" && $1 != "" { print $1; exit }')"
if [[ -z "${iface}" ]]; then
	echo "detect-wifi-iface: no Wi-Fi device found" >&2
	exit 1
fi

printf '%s\n' "${iface}"
