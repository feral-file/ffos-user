#!/usr/bin/env python3
"""Captive portal HTTP server for FF1 Wi-Fi provisioning."""

from __future__ import annotations

import argparse
import json
import mimetypes
import os
import subprocess
import sys
import threading
import time
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

STATE_DIR = Path(
    os.environ.get("CAPTIVE_PORTAL_STATE_DIR", "/home/feralfile/.state/captive-portal")
)
SUCCESS_FILE = STATE_DIR / "connect-succeeded"
CONNECT_STATE_FILE = STATE_DIR / "connect-job.json"
# Keep hotspot up briefly after POST so the phone can show "connecting" before the
# single-radio stack drops AP mode for nmcli join.
CONNECT_GRACE_SEC = float(os.environ.get("CAPTIVE_PORTAL_CONNECT_GRACE_SEC", "2"))
CONNECT_STALE_SEC = int(os.environ.get("CAPTIVE_PORTAL_CONNECT_STALE_SEC", "120"))
INSTALL_ROOT = Path(
    os.environ.get("CAPTIVE_PORTAL_INSTALL_ROOT", "/opt/feral/captive-portal")
)
WEB_ROOT = INSTALL_ROOT / "web"
WIFI_MANAGER = INSTALL_ROOT / "scripts" / "wifi-manager.sh"
PORT = int(os.environ.get("CAPTIVE_PORTAL_PORT", "8090"))
CONN_NAME = os.environ.get("CAPTIVE_PORTAL_CONN_NAME", "FeralCaptivePortal")
STOP_ON_SUCCESS = os.environ.get("CAPTIVE_PORTAL_STOP_ON_SUCCESS", "1") == "1"

# OS captive-network detection probes should bounce users into the setup UI.
CAPTIVE_PORTAL_PATHS = {
    "/generate_204",
    "/gen_204",
    "/hotspot-detect.html",
    "/library/test/success.html",
    "/connecttest.txt",
    "/ncsi.txt",
    "/redirect",
}

CONNECT_LOCK = threading.Lock()
STATE_LOCK = threading.Lock()


def hostname() -> str:
    try:
        return Path("/etc/hostname").read_text(encoding="utf-8").strip() or "FF1"
    except OSError:
        return "FF1"


def run_wifi_manager(*args: str, timeout: int = 120) -> tuple[int, str]:
    env = os.environ.copy()
    result = subprocess.run(
        [str(WIFI_MANAGER), *args],
        capture_output=True,
        text=True,
        timeout=timeout,
        env=env,
        check=False,
    )
    output = (result.stdout or "").strip()
    if not output and result.stderr:
        output = result.stderr.strip()
    return result.returncode, output


def stop_hotspot() -> None:
    subprocess.run(
        ["nmcli", "connection", "down", CONN_NAME],
        capture_output=True,
        text=True,
        check=False,
    )


def start_hotspot() -> bool:
    iface = os.environ.get("CAPTIVE_PORTAL_WIFI_IFACE", "")
    if not iface:
        return False
    result = subprocess.run(
        ["nmcli", "connection", "up", CONN_NAME, "ifname", iface],
        capture_output=True,
        text=True,
        check=False,
    )
    return result.returncode == 0


def read_connect_state() -> dict:
    if not CONNECT_STATE_FILE.is_file():
        return {"phase": "idle"}
    try:
        payload = json.loads(CONNECT_STATE_FILE.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {"phase": "idle"}
    if not isinstance(payload, dict):
        return {"phase": "idle"}
    payload.setdefault("phase", "idle")
    return payload


def write_connect_state(payload: dict) -> None:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    CONNECT_STATE_FILE.write_text(
        json.dumps(payload, separators=(",", ":")) + "\n",
        encoding="utf-8",
    )


def clear_connect_state() -> None:
    CONNECT_STATE_FILE.unlink(missing_ok=True)


def normalize_connect_state(state: dict) -> dict:
    phase = str(state.get("phase", "idle"))
    if phase != "connecting":
        return state

    started_at = float(state.get("started_at", 0))
    if started_at and time.time() - started_at > CONNECT_STALE_SEC:
        stale = {
            "phase": "failed",
            "ssid": state.get("ssid", ""),
            "error": "Connection timed out. Try again.",
            "code": "timeout",
        }
        write_connect_state(stale)
        return stale
    return state


def run_connect_job(server: ThreadingHTTPServer, ssid: str, password: str) -> None:
    time.sleep(CONNECT_GRACE_SEC)

    with CONNECT_LOCK:
        stop_hotspot()
        _code, output = run_wifi_manager("connect", ssid, password, timeout=90)
        try:
            result = json.loads(output or "{}")
        except json.JSONDecodeError:
            result = {"ok": False, "error": output or "connect failed"}

        if result.get("ok"):
            STATE_DIR.mkdir(parents=True, exist_ok=True)
            SUCCESS_FILE.write_text("ok\n", encoding="utf-8")
            write_connect_state({"phase": "success", "ssid": ssid})
            if STOP_ON_SUCCESS:
                threading.Thread(target=server.shutdown, daemon=True).start()
            return

        if not start_hotspot():
            write_connect_state(
                {
                    "phase": "failed",
                    "ssid": ssid,
                    "error": "Connection failed and hotspot could not be restored",
                    "code": "hotspot_restore_failed",
                }
            )
            return

        write_connect_state(
            {
                "phase": "failed",
                "ssid": ssid,
                "error": result.get("error") or "connection failed",
                "code": result.get("code") or "connection_failed",
            }
        )


class PortalHandler(BaseHTTPRequestHandler):
    server_version = "FeralCaptivePortal/0.1"

    def log_message(self, fmt: str, *args) -> None:
        sys.stderr.write("[portal-server] " + (fmt % args) + "\n")

    def _send_json(self, status: HTTPStatus, payload: object) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(body)

    def _redirect_home(self) -> None:
        self.send_response(HTTPStatus.FOUND)
        self.send_header("Location", "/")
        self.send_header("Cache-Control", "no-store")
        self.end_headers()

    def _serve_file(self, relative_path: str) -> None:
        safe_path = Path(relative_path.lstrip("/"))
        if safe_path.parts and ".." in safe_path.parts:
            self.send_error(HTTPStatus.NOT_FOUND)
            return

        target = WEB_ROOT / safe_path
        if target.is_dir():
            target = target / "index.html"
        if not target.is_file():
            self.send_error(HTTPStatus.NOT_FOUND)
            return

        content = target.read_bytes()
        mime, _ = mimetypes.guess_type(str(target))
        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", mime or "application/octet-stream")
        self.send_header("Content-Length", str(len(content)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(content)

    def do_OPTIONS(self) -> None:
        self.send_response(HTTPStatus.NO_CONTENT)
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type")
        self.end_headers()

    def do_GET(self) -> None:
        parsed = urlparse(self.path)
        path = parsed.path or "/"

        if path in CAPTIVE_PORTAL_PATHS:
            self._redirect_home()
            return

        if path == "/api/device":
            self._send_json(
                HTTPStatus.OK,
                {
                    "device_id": hostname(),
                    "hotspot_ssid": hostname(),
                    "hotspot_password": os.environ.get(
                        "CAPTIVE_PORTAL_HOTSPOT_PASSWORD", "feralfile-setup"
                    ),
                    "conn_name": CONN_NAME,
                },
            )
            return

        if path == "/api/networks":
            code, output = run_wifi_manager("list", "yes", timeout=45)
            if code != 0:
                self._send_json(
                    HTTPStatus.INTERNAL_SERVER_ERROR,
                    {"error": output or "scan failed"},
                )
                return
            try:
                networks = json.loads(output or "[]")
            except json.JSONDecodeError:
                self._send_json(
                    HTTPStatus.INTERNAL_SERVER_ERROR,
                    {"error": "invalid scan payload"},
                )
                return
            self._send_json(HTTPStatus.OK, {"networks": networks})
            return

        if path == "/api/status":
            code, output = run_wifi_manager("status", timeout=10)
            if code != 0:
                self._send_json(
                    HTTPStatus.INTERNAL_SERVER_ERROR,
                    {"error": output or "status failed"},
                )
                return
            try:
                payload = json.loads(output or "{}")
            except json.JSONDecodeError:
                self._send_json(
                    HTTPStatus.INTERNAL_SERVER_ERROR,
                    {"error": "invalid status payload"},
                )
                return
            self._send_json(HTTPStatus.OK, payload)
            return

        if path == "/api/connect/status":
            with STATE_LOCK:
                state = normalize_connect_state(read_connect_state())
            self._send_json(HTTPStatus.OK, state)
            return

        if path == "/":
            self._serve_file("index.html")
            return

        self._serve_file(path)

    def do_POST(self) -> None:
        parsed = urlparse(self.path)
        path = parsed.path or ""

        if path == "/api/connect/reset":
            with STATE_LOCK:
                clear_connect_state()
            self._send_json(HTTPStatus.OK, {"ok": True, "phase": "idle"})
            return

        if path != "/api/connect":
            self.send_error(HTTPStatus.NOT_FOUND)
            return

        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length > 0 else b"{}"
        try:
            payload = json.loads(raw.decode("utf-8"))
        except json.JSONDecodeError:
            self._send_json(HTTPStatus.BAD_REQUEST, {"ok": False, "error": "invalid json"})
            return

        ssid = str(payload.get("ssid", "")).strip()
        password = str(payload.get("password", ""))

        if not ssid:
            self._send_json(HTTPStatus.BAD_REQUEST, {"ok": False, "error": "ssid is required"})
            return

        with STATE_LOCK:
            current = normalize_connect_state(read_connect_state())
            if current.get("phase") == "connecting":
                self._send_json(
                    HTTPStatus.CONFLICT,
                    {
                        "ok": False,
                        "error": "A connection attempt is already in progress",
                        "code": "connect_in_progress",
                        "ssid": current.get("ssid", ""),
                    },
                )
                return

            write_connect_state(
                {
                    "phase": "connecting",
                    "ssid": ssid,
                    "started_at": time.time(),
                }
            )

        threading.Thread(
            target=run_connect_job,
            args=(self.server, ssid, password),
            daemon=True,
        ).start()

        self._send_json(
            HTTPStatus.ACCEPTED,
            {
                "ok": True,
                "accepted": True,
                "phase": "connecting",
                "ssid": ssid,
                "message": (
                    "FF1 is joining the network. This page may disconnect briefly; "
                    "that is expected on single-radio hardware."
                ),
            },
        )


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=PORT)
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if not WIFI_MANAGER.is_file():
        print(f"wifi manager not found: {WIFI_MANAGER}", file=sys.stderr)
        return 1
    if not WEB_ROOT.is_dir():
        print(f"web root not found: {WEB_ROOT}", file=sys.stderr)
        return 1

    server = ThreadingHTTPServer((args.host, args.port), PortalHandler)
    print(
        f"[portal-server] listening on {args.host}:{args.port} web={WEB_ROOT}",
        file=sys.stderr,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
