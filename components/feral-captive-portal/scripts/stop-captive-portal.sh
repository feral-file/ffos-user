#!/usr/bin/env bash
# Stop captive portal + hotspot and restore normal Wi-Fi.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=captive-portal-common.sh
source "${SCRIPT_DIR}/captive-portal-common.sh"

if [[ -f /home/feralfile/.config/feral-captive-portal.env ]]; then
	# shellcheck disable=SC1091
	source /home/feralfile/.config/feral-captive-portal.env
fi

captive_portal_stop_server
captive_portal_restore_wifi
captive_portal_clear_session_state
captive_portal_log "captive portal stopped; normal Wi-Fi restore attempted"
