#!/usr/bin/env bash
# Deploy feral-captive-portal to an FF1 device.
# Usage: ./install.sh [device-hostname]
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET_HOST="${1:-}"
INSTALL_ROOT="/opt/feral/captive-portal"
REMOTE_USER="feralfile"
REMOTE_PASS="portal"

log() {
	printf '[install-captive-portal] %s\n' "$*"
}

require_local() {
	if ! command -v rsync >/dev/null 2>&1; then
		log "rsync is required locally"
		exit 1
	fi
}

install_remote() {
	local host="$1"
	local staging="/home/feralfile/.cache/captive-portal-staging"
	log "syncing package to ${host}:${staging}"
	sshpass -p "${REMOTE_PASS}" rsync -av --delete \
		--exclude '.DS_Store' \
		"${ROOT_DIR}/" "${REMOTE_USER}@${host}:${staging}/"

	log "installing dnsmasq drop-in and fixing permissions"
	sshpass -p "${REMOTE_PASS}" ssh -o StrictHostKeyChecking=no "${REMOTE_USER}@${host}" bash -s <<EOF
set -euo pipefail
sudo mkdir -p ${INSTALL_ROOT}
sudo rsync -av --delete ${staging}/ ${INSTALL_ROOT}/
sudo chmod 755 ${INSTALL_ROOT}/scripts/*.sh
sudo mkdir -p /etc/NetworkManager/dnsmasq-shared.d
sudo cp ${INSTALL_ROOT}/config/dnsmasq-shared.d/feral-captive-portal.conf \
  /etc/NetworkManager/dnsmasq-shared.d/feral-captive-portal.conf
sudo pacman -Sy --needed --noconfirm dnsmasq python3 networkmanager
mkdir -p /home/feralfile/.logs /home/feralfile/.state/captive-portal
mkdir -p /home/feralfile/.config/systemd/user
cp ${INSTALL_ROOT}/systemd/feral-captive-portal.service /home/feralfile/.config/systemd/user/
cp ${INSTALL_ROOT}/systemd/feral-captive-portal-boot-cleanup.service /home/feralfile/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable feral-captive-portal-boot-cleanup.service
sudo systemctl restart NetworkManager
EOF
	log "installed on ${host}"
	log "start hotspot: ssh ${REMOTE_USER}@${host} '/opt/feral/captive-portal/scripts/start-captive-portal.sh'"
	log "stop hotspot:  ssh ${REMOTE_USER}@${host} '/opt/feral/captive-portal/scripts/stop-captive-portal.sh'"
}

install_local() {
	log "installing to local ${INSTALL_ROOT}"
	sudo mkdir -p "${INSTALL_ROOT}"
	sudo rsync -av --delete "${ROOT_DIR}/" "${INSTALL_ROOT}/"
	sudo chmod 755 "${INSTALL_ROOT}/scripts/"*.sh
	sudo mkdir -p /etc/NetworkManager/dnsmasq-shared.d
	sudo cp "${INSTALL_ROOT}/config/dnsmasq-shared.d/feral-captive-portal.conf" \
		/etc/NetworkManager/dnsmasq-shared.d/feral-captive-portal.conf
	mkdir -p /home/feralfile/.logs /home/feralfile/.state/captive-portal
	mkdir -p /home/feralfile/.config/systemd/user
	cp "${INSTALL_ROOT}/systemd/feral-captive-portal.service" /home/feralfile/.config/systemd/user/
	cp "${INSTALL_ROOT}/systemd/feral-captive-portal-boot-cleanup.service" /home/feralfile/.config/systemd/user/
	systemctl --user daemon-reload
	systemctl --user enable feral-captive-portal-boot-cleanup.service
	log "local install complete"
}

require_local

if [[ -n "${TARGET_HOST}" ]]; then
	if ! command -v sshpass >/dev/null 2>&1; then
		log "sshpass is required for remote install"
		exit 1
	fi
	install_remote "${TARGET_HOST}"
else
	install_local
fi
