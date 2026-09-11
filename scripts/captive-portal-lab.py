#!/usr/bin/env python3
"""Temporary probe-response experiment, not a replacement provisioning server."""

import argparse
import datetime
import html
import http.server
import ipaddress
import json
import os
import re
import secrets
import signal
import subprocess
import threading
import time
import urllib.parse


PROBES = {
    "/hotspot-detect.html", "/library/test/success.html", "/generate_204",
    "/gen_204", "/connecttest.txt", "/ncsi.txt", "/redirect",
}


def portal_url(value):
    parsed = urllib.parse.urlsplit(value)
    if (any(ord(c) < 33 for c in value) or parsed.scheme != "http"
            or parsed.username or parsed.password or parsed.query or parsed.fragment
            or parsed.path not in ("", "/")):
        raise argparse.ArgumentTypeError("use the hotspot's numeric HTTP root URL")
    try:
        address = ipaddress.IPv4Address(parsed.hostname)
        port = parsed.port
    except (ValueError, TypeError) as error:
        raise argparse.ArgumentTypeError("a numeric IPv4 portal address is required") from error
    if address.is_unspecified or address.is_multicast or port == 0:
        raise argparse.ArgumentTypeError("invalid portal address")
    return urllib.parse.urlunsplit(("http", parsed.netloc, "/", "", ""))


def bounded_duration(value):
    seconds = int(value)
    if not 1 <= seconds <= 300:
        raise argparse.ArgumentTypeError("duration must be 1–300 seconds")
    return seconds


def redirect_rules(table, interface, gateway, port, duration):
    # An expiring port set restores the ordinary portal even if this process
    # is killed without running finally. This table never modifies FF1's rules.
    return f"""create table ip {table}
add set ip {table} ports {{ type inet_service; flags timeout; }}
add element ip {table} ports {{ 80 timeout {duration}s }}
add chain ip {table} prerouting {{ type nat hook prerouting priority -110; policy accept; }}
add rule ip {table} prerouting iifname "{interface}" ip daddr {{ {gateway}, 192.0.2.1 }} tcp dport @ports redirect to :{port}
"""


def active_ap_gateway(interface):
    if not re.fullmatch(r"[a-zA-Z0-9_.-]{1,15}", interface):
        raise ValueError("invalid interface name")
    def nmcli(*args):
        return subprocess.check_output(["nmcli", *args], text=True, timeout=5).strip()
    uuid = nmcli("-g", "GENERAL.CON-UUID", "device", "show", interface)
    mode = nmcli("-g", "802-11-wireless.mode", "connection", "show", "uuid", uuid)
    ssid = nmcli("-g", "802-11-wireless.ssid", "connection", "show", "uuid", uuid)
    if mode != "ap" or not ssid.startswith("FF1-"):
        raise ValueError("redirect requires an active FF1 setup hotspot")
    addresses = nmcli("-g", "IP4.ADDRESS", "device", "show", interface).splitlines()
    return str(ipaddress.IPv4Interface(addresses[0]).ip)


def wait_for_ap(interface, seconds):
    deadline = time.monotonic() + seconds
    while True:
        try:
            return active_ap_gateway(interface)
        except (ValueError, IndexError, subprocess.SubprocessError):
            if time.monotonic() >= deadline:
                raise
            time.sleep(min(1, max(0, deadline - time.monotonic())))


class LabServer(http.server.ThreadingHTTPServer):
    allow_reuse_address = True

    def __init__(self, address, mode, target, duration):
        self.mode = mode
        self.target = target
        self.started = time.monotonic()
        self.deadline = self.started + duration
        self.stop_requested = False
        self.first_probe = None
        self.event_lock = threading.Lock()
        self.beacon_path = "/__ff1_lab_visible/" + secrets.token_hex(16)
        self.page = self.make_page()
        super().__init__(address, Handler)
        self.timeout = 0.2

    def get_request(self):
        connection, address = super().get_request()
        connection.settimeout(max(0.01, min(2, self.deadline - time.monotonic())))
        return connection, address

    def event(self, event, **fields):
        with self.event_lock:
            print(json.dumps({
                "time": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                "elapsed_ms": round((time.monotonic() - self.started) * 1000),
                "mode": self.mode, "event": event, **fields,
            }), flush=True)

    def handle_error(self, *_):
        self.event("request_error")

    def make_page(self):
        return ("""<!doctype html><html><head><meta name="viewport"
content="width=device-width,initial-scale=1"><title>FF1 connection test</title>
</head><body><h1>FF1 connection test</h1><p>The test page opened.</p>
<p>This test does not collect Wi-Fi details. Close this window when asked.</p>
<script>
let sent = false;
function visible() {
  if (sent || document.visibilityState !== 'visible') return;
  sent = true;
  fetch(BEACON, {cache: 'no-store', keepalive: true}).catch(() => {});
}
document.addEventListener('visibilitychange', visible);
visible();
</script></body></html>""".replace("BEACON", json.dumps(self.beacon_path))).encode()


class Handler(http.server.BaseHTTPRequestHandler):
    server_version = "FF1PortalLab"
    sys_version = ""

    def log_message(self, *_):
        # BaseHTTPRequestHandler otherwise records arbitrary URLs and headers
        # from unrelated phone traffic. Only classified events are useful here.
        pass

    def reply(self, status, body=b"", location=None):
        self.send_response(status)
        self.send_header("Cache-Control", "no-store")
        self.send_header("Connection", "close")
        self.send_header("Content-Length", str(len(body)))
        if body:
            self.send_header("Content-Type", "text/html; charset=utf-8")
        if location is not None:
            self.send_header("Location", location)
        self.end_headers()
        self.close_connection = True
        if self.command != "HEAD":
            self.wfile.write(body)

    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        agent = self.headers.get("User-Agent", "")
        client = "captive_probe" if "CaptiveNetworkSupport" in agent else "other"
        if path == self.server.beacon_path:
            # This proves visible-page JavaScript ran, not that iOS auto-opened
            # its native sheet. The tester must still report what they saw.
            if self.command != "HEAD":
                first = self.server.first_probe
                delay = None if first is None else round((time.monotonic() - first) * 1000)
                self.server.event("visible_page_beacon", first_probe_to_beacon_ms=delay)
            return self.reply(204)
        probe = path in PROBES or client == "captive_probe"
        if probe and self.command != "HEAD":
            with self.server.event_lock:
                if self.server.first_probe is None:
                    self.server.first_probe = time.monotonic()
        self.server.event("request", role="probe" if probe else "page", client=client,
                          method=self.command)
        if path == "/" or (probe and self.server.mode == "html"):
            return self.reply(200, self.server.page)
        target = self.server.target if self.server.mode == "absolute" else "/"
        body = f'<a href="{html.escape(target, quote=True)}">Found</a>.\n\n'.encode()
        return self.reply(302, body, target)

    do_HEAD = do_GET

    def do_POST(self):
        # Never read or forward a form body. The real setup server stays on :80
        # and becomes reachable again when the experiment's redirect expires.
        self.server.event("submission_rejected")
        self.reply(405)

    do_PUT = do_DELETE = do_PATCH = do_POST


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", required=True, choices=("relative", "absolute", "html"))
    parser.add_argument("--portal-url", type=portal_url,
                        help="required for local tests; otherwise discovered from the hotspot")
    parser.add_argument("--bind", default="127.0.0.1", type=ipaddress.IPv4Address)
    parser.add_argument("--port", type=int, default=18080)
    parser.add_argument("--duration", type=bounded_duration, default=120)
    parser.add_argument("--redirect-interface", help="opt in to temporary FF1 hotspot HTTP interception")
    parser.add_argument("--wait-for-ap", type=int, default=0,
                        help="wait up to 120 seconds for setup mode, so SSH can start this beforehand")
    args = parser.parse_args()
    if not 1024 <= args.port <= 65535:
        parser.error("port must be 1024–65535; do not replace the production portal listener")
    if not 0 <= args.wait_for_ap <= 120 or (args.wait_for_ap and not args.redirect_interface):
        parser.error("wait-for-ap must be 0–120 and requires redirect-interface")
    if args.redirect_interface:
        if os.geteuid() != 0 or str(args.bind) != "0.0.0.0":
            parser.error("hotspot redirection requires root and --bind 0.0.0.0")
        gateway = wait_for_ap(args.redirect_interface, args.wait_for_ap)
        if args.portal_url is not None and args.portal_url != f"http://{gateway}/":
            parser.error("portal-url must be the active hotspot's HTTP root on port 80")
        args.portal_url = f"http://{gateway}/"
    elif args.portal_url is None:
        parser.error("portal-url is required without redirect-interface")
    table = "ff1_portal_lab_" + secrets.token_hex(6)
    installed = False
    with LabServer((str(args.bind), args.port), args.mode, args.portal_url,
                   args.duration + (3 if args.redirect_interface else 0)) as server:
        def stop(*_):
            server.stop_requested = True
        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGINT, stop)
        try:
            if args.redirect_interface:
                rules = redirect_rules(table, args.redirect_interface, gateway,
                                       args.port, args.duration)
                subprocess.run(["nft", "-f", "-"], input=rules, text=True, check=True, timeout=5)
                installed = True
                server.deadline = time.monotonic() + args.duration + 3
            server.event("started", duration_seconds=args.duration,
                         hotspot_redirect=installed, port=server.server_port)
            while not server.stop_requested and time.monotonic() < server.deadline:
                server.handle_request()
        finally:
            if installed:
                subprocess.run(["nft", "delete", "table", "ip", table], check=True, timeout=5)
            server.event("stopped")


if __name__ == "__main__":
    main()
