#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ui_dir="$repo_root/components/mint-pairing-ui"
index_html="$ui_dir/index.html"
qr_lib="$ui_dir/js/qrcode.min.js"

if ! command -v node >/dev/null 2>&1; then
  echo "mint-pairing-ui: Node.js is required for QR page smoke tests; install it with brew install node or apt install nodejs." >&2
  exit 1
fi

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

node - "$index_html" "$qr_lib" <<'NODE'
const fs = require("fs");
const vm = require("vm");
const html = fs.readFileSync(process.argv[2], "utf8");
const qrLibrary = fs.readFileSync(process.argv[3], "utf8");
const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map((match) => match[1]);
if (scripts.length !== 1) {
  throw new Error(`expected one inline render script, got ${scripts.length}`);
}

class Element {
  constructor(tagName, id = "") {
    this.id = id;
    this.tagName = tagName;
    this.children = [];
    this.hidden = false;
    this.style = {};
    this.textContent = "";
    this.title = "";
    this.offsetWidth = 399;
    this.offsetHeight = 399;
  }
  set innerHTML(value) {
    this.children = [];
    this._innerHTML = value;
    if (value.trim().startsWith("<table")) {
      this.children.push(new Element("table"));
    }
  }
  get innerHTML() {
    return this._innerHTML || "";
  }
  get childNodes() {
    return this.children;
  }
  get lastChild() {
    return this.children[this.children.length - 1] || null;
  }
  appendChild(child) {
    this.children.push(child);
    return child;
  }
  hasChildNodes() {
    return this.children.length > 0;
  }
  removeChild(child) {
    const index = this.children.indexOf(child);
    if (index >= 0) {
      this.children.splice(index, 1);
    }
    return child;
  }
  setAttribute(name, value) {
    this[name] = value;
  }
  setAttributeNS(_namespace, name, value) {
    this.setAttribute(name, value);
  }
}

const elements = {
  qrcode: new Element("div", "qrcode"),
  pairingCode: new Element("div", "pairingCode"),
  error: new Element("div", "error"),
};
let domReady;
const context = {
  URLSearchParams,
  navigator: { userAgent: "node" },
  window: {
    location: { search: "?pairing_code=PAIR-123" },
    innerWidth: 1280,
    innerHeight: 720,
    addEventListener(event, callback) {
      if (event !== "resize") {
        throw new Error(`unexpected window event ${event}`);
      }
      this.resize = callback;
    },
  },
  document: {
    documentElement: new Element("html"),
    getElementById(id) {
      const element = elements[id];
      if (!element) {
        throw new Error(`unexpected element id ${id}`);
      }
      return element;
    },
    createElement(tagName) {
      return new Element(tagName);
    },
    createElementNS(_namespace, tagName) {
      return new Element(tagName);
    },
    addEventListener(event, callback) {
      if (event !== "DOMContentLoaded") {
        throw new Error(`unexpected document event ${event}`);
      }
      domReady = callback;
    },
  },
};
context.window.document = context.document;
context.globalThis = context;
vm.createContext(context);

vm.runInContext(qrLibrary, context, { filename: "qrcode.min.js" });
if (typeof context.QRCode !== "function" || typeof context.QRCode.prototype.makeCode !== "function") {
  throw new Error("bundled QRCode library did not initialize");
}

vm.runInContext(scripts[0], context, { filename: "index.html inline render script" });
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
if (elements.qrcode.title !== "PAIR-123") {
  throw new Error(`QR library did not receive expected text, got ${elements.qrcode.title}`);
}
if (elements.qrcode.children.length !== 1 || elements.qrcode.children[0].tagName !== "table") {
  throw new Error("real QR library did not produce table output");
}
const darkCellCount = (elements.qrcode.innerHTML.match(/background-color:#000000/g) || []).length;
const lightCellCount = (elements.qrcode.innerHTML.match(/background-color:#ffffff/g) || []).length;
if (darkCellCount < 100 || lightCellCount < 100) {
  throw new Error(`QR table output is too small: ${darkCellCount} dark cells, ${lightCellCount} light cells`);
}
NODE

echo "test-mint-pairing-ui: OK"
