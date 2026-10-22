#!/usr/bin/env python3
"""HTTP contract and lifetime checks; these cannot verify iOS presentation policy."""

import argparse
import contextlib
import http.client
import importlib.util
import io
import json
from pathlib import Path
import re
import socket
import subprocess
import sys
import threading
import time
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name("captive-portal-lab.py")
spec = importlib.util.spec_from_file_location("captive_portal_lab", SCRIPT)
lab = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lab)


@contextlib.contextmanager
def running(mode):
    output = io.StringIO()
    with contextlib.redirect_stdout(output):
        with lab.LabServer(("127.0.0.1", 0), mode, "http://10.42.0.1/", 30) as server:
            thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
            thread.start()
            try:
                yield server, output
            finally:
                server.shutdown()
                thread.join(timeout=2)


def request(server, method, path, body=None, headers=None):
    client = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=2)
    # Match the HTTP/1.0 probe observed in the actual iPhone log.
    client._http_vsn = 10
    client._http_vsn_str = "HTTP/1.0"
    client.request(method, path, body=body, headers=headers or {})
    response = client.getresponse()
    result = response.status, dict(response.getheaders()), response.read()
    client.close()
    return result


class ProbeResponseTests(unittest.TestCase):
    def test_same_probe_has_three_distinct_responses(self):
        for mode, expected_status, expected_location in (
            ("relative", 302, "/"),
            ("absolute", 302, "http://10.42.0.1/"),
            ("html", 200, None),
        ):
            with self.subTest(mode=mode), running(mode) as (server, _):
                status, headers, body = request(server, "GET", "/hotspot-detect.html", headers={
                    "User-Agent": "CaptiveNetworkSupport-514.160.1.0.1 wispr",
                })
                self.assertEqual(status, expected_status)
                self.assertEqual(headers.get("Location"), expected_location)
                self.assertEqual(headers["Cache-Control"], "no-store")
                self.assertEqual(headers["Connection"], "close")
                if mode == "html":
                    self.assertIn(b"FF1 connection test", body)
                self.assertNotIn(b"<TITLE>Success</TITLE>", body)

    def test_page_load_does_not_itself_claim_visible_ui(self):
        with running("relative") as (server, output):
            request(server, "GET", "/hotspot-detect.html")
            status, _, body = request(server, "GET", "/")
            self.assertEqual(status, 200)
            self.assertIn(b"document.visibilityState", body)
            self.assertNotIn("visible_page_beacon", output.getvalue())
            path = re.search(rb'fetch\("([^"]+)"', body).group(1).decode()
            request(server, "HEAD", path)
            self.assertNotIn("visible_page_beacon", output.getvalue())
            self.assertEqual(request(server, "GET", path)[0], 204)
            event = json.loads(output.getvalue().splitlines()[-1])
            self.assertEqual(event["event"], "visible_page_beacon")
            self.assertGreaterEqual(event["first_probe_to_beacon_ms"], 0)

    def test_credential_submission_and_unrelated_urls_are_never_logged(self):
        with running("absolute") as (server, output):
            self.assertEqual(request(server, "POST", "/connect", body="password=private-password")[0], 405)
            request(server, "GET", "/unrelated-path?secret=private-query", headers={"User-Agent": "private-agent"})
            for private in ("private-password", "unrelated-path", "private-query", "private-agent"):
                self.assertNotIn(private, output.getvalue())

    def test_head_has_the_same_headers_without_a_body(self):
        with running("html") as (server, _):
            status, headers, body = request(server, "HEAD", "/hotspot-detect.html")
            self.assertEqual(status, 200)
            self.assertGreater(int(headers["Content-Length"]), 0)
            self.assertEqual(body, b"")
            self.assertIsNone(server.first_probe)


class LifetimeAndRedirectTests(unittest.TestCase):
    def test_station_connection_cannot_be_intercepted(self):
        with mock.patch.object(lab.subprocess, "check_output", side_effect=["uuid", "infrastructure", "Home"]):
            with self.assertRaisesRegex(ValueError, "active FF1 setup hotspot"):
                lab.active_ap_gateway("wlp2s0")

    def test_redirect_transaction_expires_and_only_captures_setup_http(self):
        rules = lab.redirect_rules("ff1_portal_lab_test", "wlp2s0", "10.42.0.1", 18080, 60)
        self.assertTrue(rules.startswith("create table ip ff1_portal_lab_test\n"))
        self.assertIn("80 timeout 60s", rules)
        self.assertIn('iifname "wlp2s0" ip daddr { 10.42.0.1, 192.0.2.1 }', rules)
        self.assertIn("tcp dport @ports redirect to :18080", rules)
        self.assertNotIn("443", rules)
        self.assertNotIn("flush", rules)

    def test_only_numeric_http_root_targets_are_accepted(self):
        self.assertEqual(lab.portal_url("http://10.42.7.1"), "http://10.42.7.1/")
        for invalid in ("https://10.42.0.1", "http://example.com", "http://10.42.0.1/path",
                        "http://user:pass@10.42.0.1", "http://10.42.0.1/?x=1", "http://0.0.0.0"):
            with self.subTest(invalid=invalid), self.assertRaises((ValueError, argparse.ArgumentTypeError)):
                lab.portal_url(invalid)

    def test_process_exits_on_deadline_even_with_an_incomplete_client_request(self):
        reservation = socket.socket()
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
        reservation.close()
        started = time.monotonic()
        process = subprocess.Popen([
            sys.executable, "-u", str(SCRIPT), "--mode", "relative", "--duration", "1",
            "--portal-url", "http://10.42.0.1", "--port", str(port),
        ], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            first = json.loads(process.stdout.readline())
            self.assertEqual(first["event"], "started")
            with socket.create_connection(("127.0.0.1", port), timeout=1) as client:
                client.sendall(b"GET / HTTP/1.0\r\nHost:")
                stdout, stderr = process.communicate(timeout=4)
            self.assertEqual(process.returncode, 0, stderr)
            self.assertEqual(json.loads(stdout.splitlines()[-1])["event"], "stopped")
            self.assertLess(time.monotonic() - started, 3)
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate()


if __name__ == "__main__":
    unittest.main()
