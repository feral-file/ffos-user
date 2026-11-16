#!/usr/bin/env python3
"""Exercise real TCP redirection and recovery using two isolated Linux netns."""

import argparse
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import sys
import time


LAB = Path(__file__).with_name("captive-portal-lab.py").resolve()
GATEWAY = "10.42.0.1"
CAPTIVE = "192.0.2.1"


def run(*args, data=None):
    return subprocess.check_output(args, input=data, text=True, timeout=10)


def ready(process):
    with selectors.DefaultSelector() as selector:
        selector.register(process.stdout, selectors.EVENT_READ)
        if not selector.select(10):
            raise AssertionError("server did not become ready within 10 seconds")
    line = process.stdout.readline()
    if not line:
        raise AssertionError("server exited before readiness: " + process.stderr.read())
    return line


def stop(process, sig=signal.SIGTERM):
    if process is not None and process.poll() is None:
        process.send_signal(sig)
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


class NetworkTrial:
    def __init__(self, fixture):
        self.client = None
        self.portal = None
        self.lab = None
        self.fixture = fixture

    def __enter__(self):
        try:
            self.client = subprocess.Popen(["unshare", "--net", "--", "sleep", "120"])
            current = os.readlink("/proc/self/ns/net")
            deadline = time.monotonic() + 5
            while os.readlink(f"/proc/{self.client.pid}/ns/net") == current:
                if self.client.poll() is not None or time.monotonic() > deadline:
                    raise AssertionError("client network namespace did not start")
                time.sleep(0.01)
            run("ip", "link", "set", "lo", "up")
            run("ip", "link", "add", "wlp2s0", "type", "veth", "peer", "name",
                "labclient0", "netns", str(self.client.pid))
            run("ip", "address", "add", GATEWAY + "/24", "dev", "wlp2s0")
            run("ip", "link", "set", "wlp2s0", "up")
            self.in_client("ip", "link", "set", "lo", "up")
            self.in_client("ip", "address", "add", "10.42.0.2/24", "dev", "labclient0")
            self.in_client("ip", "link", "set", "labclient0", "up")
            self.in_client("ip", "route", "add", "default", "via", GATEWAY)
            # Match the production captive chain and its -100 priority. The
            # lab's -110 interception must run before this existing redirect.
            run("nft", "-f", "-", data="""create table ip feral_captive
add chain ip feral_captive prerouting { type nat hook prerouting priority -100; policy accept; }
add rule ip feral_captive prerouting ip daddr 192.0.2.1 tcp dport { 80, 443 } redirect
""")
            self.original_rules = run("nft", "list", "table", "ip", "feral_captive")
            self.portal = subprocess.Popen([self.fixture], stdout=subprocess.PIPE,
                                           stderr=subprocess.PIPE, text=True)
            assert ready(self.portal).strip() == "ready"
            baseline = self.get(GATEWAY, "/")
            assert baseline["status"] == 200 and "<form" in baseline["body"]
            self.title = re.search(r"<title>(.*?)</title>", baseline["body"]).group(1)
            self.assert_normal()
            return self
        except BaseException:
            self.__exit__(None, None, None)
            raise

    def __exit__(self, *_):
        stop(self.lab)
        stop(self.portal)
        stop(self.client)

    def in_client(self, *args, data=None):
        return run("nsenter", "-t", str(self.client.pid), "--net", "--", *args, data=data)

    def get(self, destination, path):
        code = """import http.client,json,sys
c=http.client.HTTPConnection(sys.argv[1],80,timeout=2)
c._http_vsn=10
c._http_vsn_str='HTTP/1.0'
c.request('GET',sys.argv[2],headers={'Host':'captive.apple.com'})
r=c.getresponse()
print(json.dumps({'status':r.status,'location':r.getheader('Location'),'body':r.read().decode()}))
c.close()
"""
        return json.loads(self.in_client(sys.executable, "-c", code, destination, path))

    def assert_normal(self):
        for address in (GATEWAY, CAPTIVE):
            probe = self.get(address, "/hotspot-detect.html")
            assert (probe["status"], probe["location"]) == (302, "/"), probe
            page = self.get(address, "/")
            assert page["status"] == 200 and f"<title>{self.title}</title>" in page["body"]
        assert run("nft", "list", "table", "ip", "feral_captive") == self.original_rules

    def start_lab(self, mode, duration):
        # NM itself remains in the host namespace. Only AP metadata is a
        # fixture; the CLI, listeners, packets, netfilter and timers are real.
        code = """import importlib.util,sys
spec=importlib.util.spec_from_file_location('lab',sys.argv[1])
lab=importlib.util.module_from_spec(spec)
spec.loader.exec_module(lab)
lab.active_ap_gateway=lambda _: '10.42.0.1'
sys.argv=sys.argv[1:]
lab.main()
"""
        self.lab = subprocess.Popen([
            sys.executable, "-B", "-u", "-c", code, str(LAB), "--mode", mode,
            "--bind", "0.0.0.0", "--redirect-interface", "wlp2s0",
            "--duration", str(duration),
        ], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        event = json.loads(ready(self.lab))
        assert event["event"] == "started" and event["hotspot_redirect"]
        self.started = time.monotonic()
        for address in (GATEWAY, CAPTIVE):
            probe = self.get(address, "/hotspot-detect.html")
            expected = {
                "relative": (302, "/"),
                "absolute": (302, "http://10.42.0.1/"),
                "html": (200, None),
            }[mode]
            assert (probe["status"], probe["location"]) == expected, probe
            page = self.get(address, "/")
            assert page["status"] == 200 and "FF1 connection test" in page["body"]

    def expire(self, duration):
        remaining = self.started + duration + 0.3 - time.monotonic()
        if remaining > 0:
            time.sleep(remaining)
        self.assert_normal()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--portal-fixture", required=True, type=Path)
    parser.add_argument("--inside-namespace", help=argparse.SUPPRESS)
    args = parser.parse_args()
    fixture = str(args.portal_fixture.resolve(strict=True))
    if os.geteuid() != 0:
        parser.error("run with sudo; the test creates isolated network namespaces")
    namespace = os.readlink("/proc/self/ns/net")
    if args.inside_namespace is None:
        os.execvp("unshare", ["unshare", "--net", "--", sys.executable, "-B", __file__,
                              "--portal-fixture", fixture, "--inside-namespace", namespace])
    # Reject accidental direct execution in a live network. This check runs
    # before adding any interface, address, route or firewall rule.
    if namespace == args.inside_namespace:
        raise SystemExit("network namespace isolation failed")
    links = json.loads(run("ip", "-j", "link"))
    if [link["ifname"] for link in links] != ["lo"] or run("nft", "list", "tables").strip():
        raise SystemExit("refusing to modify a nonempty network namespace")
    with NetworkTrial(fixture) as trial:
        for mode in ("relative", "absolute", "html"):
            trial.start_lab(mode, 3)
            trial.expire(3)
            # Recovery must happen while the lab still exists, through the
            # independent nft timeout rather than process teardown alone.
            assert trial.lab.poll() is None
            stop(trial.lab)
            assert trial.lab.returncode == 0
            assert "ff1_portal_lab_" not in run("nft", "list", "tables")
            print(mode + ": both destinations and automatic recovery passed", flush=True)
        trial.start_lab("absolute", 3)
        stop(trial.lab, signal.SIGKILL)
        trial.expire(3)
        print("SIGKILL: normal portal recovered after independent expiry", flush=True)
        trial.start_lab("html", 30)
        stop(trial.lab)
        trial.assert_normal()
        print("SIGTERM: normal portal recovered immediately", flush=True)


if __name__ == "__main__":
    main()
