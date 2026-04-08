#!/bin/bash
set -euo pipefail

OVERRIDE_PATH="/home/feralfile/.config/webapp-url"

rm -f "$OVERRIDE_PATH"
systemctl --user stop ff-player-local.service || true
systemctl --user disable ff-player-local.service || true
systemctl --user restart feral-setupd.service

echo "disabled local ff-player override"
