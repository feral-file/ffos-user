#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ui_dir="$repo_root/components/mint-pairing-ui"
index_html="$ui_dir/index.html"
qr_lib="$ui_dir/js/qrcode.min.js"

if [[ ! -s "$index_html" ]]; then
  echo "mint-pairing-ui: missing index.html" >&2
  exit 1
fi

if ! grep -q 'js/qrcode.min.js' "$index_html"; then
  echo "mint-pairing-ui: index.html must load js/qrcode.min.js" >&2
  exit 1
fi

if ! grep -q 'pairing_code' "$index_html"; then
  echo "mint-pairing-ui: index.html must read pairing_code from the URL" >&2
  exit 1
fi

if ! grep -q 'new QRCode' "$index_html"; then
  echo "mint-pairing-ui: index.html must render a QRCode" >&2
  exit 1
fi

if [[ ! -s "$qr_lib" ]]; then
  echo "mint-pairing-ui: missing js/qrcode.min.js" >&2
  exit 1
fi

echo "test-mint-pairing-ui: OK"
