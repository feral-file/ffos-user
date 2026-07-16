#!/usr/bin/env bash
# Start hotspot + captive portal manually over SSH.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

if [[ -f /home/feralfile/.config/feral-captive-portal.env ]]; then
	# shellcheck disable=SC1091
	source /home/feralfile/.config/feral-captive-portal.env
fi

export CAPTIVE_PORTAL_INSTALL_ROOT="${CAPTIVE_PORTAL_INSTALL_ROOT:-${INSTALL_ROOT}}"

log() {
	printf '[start-captive-portal] %s\n' "$*" >&2
}

# Prefer the user unit so port 80 capabilities are granted without an interactive sudo shell.
if command -v systemctl >/dev/null 2>&1 \
	&& systemctl --user start feral-captive-portal.service >/dev/null 2>&1; then
	log "started feral-captive-portal.service"
	log "logs: /home/feralfile/.logs/feral-captive-portal.log"
	log "stop with: /opt/feral/captive-portal/scripts/stop-captive-portal.sh"
	exit 0
fi

log "systemd user start failed; falling back to direct script (requires sudo for port 80)"
exec sudo -E "${INSTALL_ROOT}/scripts/feral-captive-portal.sh"
