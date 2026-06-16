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

node - "$index_html" <<'NODE'
const fs = require("fs");
const html = fs.readFileSync(process.argv[2], "utf8");
const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map((match) => match[1]);
if (scripts.length !== 1) {
  throw new Error(`expected one inline render script, got ${scripts.length}`);
}

class Element {
  constructor(id) {
    this.id = id;
    this.children = [];
    this.hidden = false;
    this.textContent = "";
  }
  set innerHTML(value) {
    this.children = [];
    this._innerHTML = value;
  }
  get innerHTML() {
    return this._innerHTML || "";
  }
  appendChild(child) {
    this.children.push(child);
    return child;
  }
}

const elements = {
  qrcode: new Element("qrcode"),
  pairingCode: new Element("pairingCode"),
  error: new Element("error"),
};
let domReady;
global.window = {
  location: { search: "?pairing_code=PAIR-123" },
  innerWidth: 1280,
  innerHeight: 720,
  addEventListener(event, callback) {
    if (event !== "resize") {
      throw new Error(`unexpected window event ${event}`);
    }
    this.resize = callback;
  },
};
global.document = {
  getElementById(id) {
    const element = elements[id];
    if (!element) {
      throw new Error(`unexpected element id ${id}`);
    }
    return element;
  },
  addEventListener(event, callback) {
    if (event !== "DOMContentLoaded") {
      throw new Error(`unexpected document event ${event}`);
    }
    domReady = callback;
  },
};
global.QRCode = function QRCode(container, options) {
  if (options.text !== "PAIR-123") {
    throw new Error(`unexpected QR text ${options.text}`);
  }
  container.appendChild({ tagName: "IMG", width: options.width, height: options.height });
};
global.QRCode.CorrectLevel = { M: "M" };

eval(scripts[0]);
if (typeof domReady !== "function") {
  throw new Error("render script did not register DOMContentLoaded");
}
domReady();

if (elements.pairingCode.textContent !== "PAIR-123") {
  throw new Error(`unexpected pairing code ${elements.pairingCode.textContent}`);
}
if (!elements.error.hidden) {
  throw new Error("error element should be hidden for a valid pairing code");
}
if (elements.qrcode.children.length !== 1 || elements.qrcode.children[0].tagName !== "IMG") {
  throw new Error("QR render did not produce image output");
}
NODE

echo "test-mint-pairing-ui: OK"
