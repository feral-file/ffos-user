#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   ./scripts/sync-to-device.sh [local-dir] [remote-dir] [remote-host]
# E.g:
#   ./scripts/sync-to-device.sh . /home/feralfile/wifi_bt_app 192.168.31.91

LOCAL_DIR="${1:-.}/"
REMOTE_USER="feralfile"
REMOTE_HOST="${3:-192.168.31.91}"
REMOTE_PASS="portal"
REMOTE_DIR="${2:-/home/${REMOTE_USER}/src/components/}"

# ensure sshpass is installed
if ! command -v sshpass &>/dev/null; then
  echo "❌ sshpass not found. Install it via: brew install sshpass"
  exit 1
fi

# build ssh wrapper
SSH_CMD="sshpass -p '${REMOTE_PASS}' ssh -o StrictHostKeyChecking=no"

# print debug info
echo "🔍 LOCAL_DIR = ${LOCAL_DIR}"
echo "🌐 REMOTE_DIR = ${REMOTE_USER}@${REMOTE_HOST}:${REMOTE_DIR}"

rsync -avz \
  --delete \
  --exclude 'target/' \
  --exclude '.git/' \
  --exclude '*.rs.bk' \
  -e "${SSH_CMD}" \
  "${LOCAL_DIR}" \
  "${REMOTE_USER}@${REMOTE_HOST}:${REMOTE_DIR}"