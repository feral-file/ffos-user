#!/usr/bin/env bash
# Boot hook: never leave hotspot active across reboot; restore saved/autoconnect Wi-Fi.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=captive-portal-common.sh
source "${SCRIPT_DIR}/captive-portal-common.sh"

if [[ -f /home/feralfile/.config/feral-captive-portal.env ]]; then
	# shellcheck disable=SC1091
	source /home/feralfile/.config/feral-captive-portal.env
fi

mkdir -p "$(captive_portal_state_dir)"

captive_portal_stop_server
captive_portal_restore_wifi
captive_portal_clear_session_state
captive_portal_log "boot cleanup complete"
