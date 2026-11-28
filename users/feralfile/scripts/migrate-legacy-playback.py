"""Read the frozen 3.0.1 kiosk profile through Chromium's own storage API.

Candidate promotion runs this as feralfile in a loopback-only network namespace.
Only the old player's origin storage is needed. Never open a saved tab, load
an artwork, or let Chromium rewrite the rollback profile.
"""
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time

from player_health import fingerprint

STATE = Path.home() / ".state/player"
INDEX = Path("IndexedDB/http_127.0.0.1_8080.indexeddb.leveldb")
LOCAL = Path("Local Storage/leveldb")
PAGE = b"""<!doctype html><script>
(async () => {
  const req = indexedDB.open("FeralFileDisplayDB");
  const db = await new Promise((resolve, reject) => {
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
  const keys = ["castInfo", "boot_playlist", "contentPolicy", "displaySettings",
                "defaultItemDuration", "dp1_scheduled_tasks"];
  const store = db.objectStoreNames.contains("keyValueStore")
    ? db.transaction("keyValueStore").objectStore("keyValueStore") : null;
  const values = await Promise.all(keys.map(key => new Promise((resolve, reject) => {
    if (!store) return resolve(localStorage.getItem(key));
    const r = store.get(key);
    r.onsuccess = () => resolve(r.result ?? localStorage.getItem(key));
    r.onerror = () => reject(r.error);
  })));
  const data = Object.fromEntries(keys.map((k, i) => [k, JSON.parse(values[i])]));
  if (data.dp1_scheduled_tasks) {
    const atMs = Date.parse(data.dp1_scheduled_tasks.scheduleTime);
    if (!Number.isFinite(atMs)) throw Error("Invalid scheduled time");
    data.dp1_scheduled_tasks.atMs = atMs;
  }
  await fetch("/export", {method: "POST", body: JSON.stringify(data)});
})().catch(() => fetch("/error", {method: "POST"}));
</script>"""


class StorageReader(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path != "/read":
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.end_headers()
        self.wfile.write(PAGE)

    def do_POST(self):
        size = int(self.headers.get("Content-Length", 0))
        if self.path == "/export" and 0 < size <= 4 * 1024 * 1024:
            self.server.export = self.rfile.read(size)
        else:
            self.server.export = b""
        self.send_response(204)
        self.end_headers()


def read_profile(source, directory):
    for relative in (INDEX, INDEX.with_suffix(".blob"), LOCAL):
        if (source / relative).is_dir():
            shutil.copytree(source / relative, directory / "Default" / relative)
    with http.server.HTTPServer(("127.0.0.1", 8080), StorageReader) as server:
        server.timeout = 1
        server.export = None
        process = subprocess.Popen([
            "chromium", "--headless", "--disable-gpu", "--disable-background-networking",
            "--no-first-run", "--no-default-browser-check", "--no-proxy-server",
            f"--user-data-dir={directory}", "http://127.0.0.1:8080/read",
        ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            deadline = time.monotonic() + 45
            while server.export is None and process.poll() is None and time.monotonic() < deadline:
                server.handle_request()
            if not server.export:
                raise RuntimeError("Could not read the legacy kiosk storage")
            return server.export
        finally:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


def main():
    source = Path(sys.argv[1])
    if not any((source / path).is_dir() for path in (INDEX, LOCAL)):
        print("skip")
        return
    with tempfile.TemporaryDirectory() as profile, tempfile.TemporaryDirectory(dir=STATE.parent) as stage:
        records = read_profile(source, Path(profile))
        # Publish only after the native host has validated every record and
        # closed SQLite (including its WAL). Failure leaves the old root available for fallback.
        result = subprocess.run(["/usr/bin/feral-display", "--import-legacy", f"--state-dir={stage}"],
                                input=records, stdout=subprocess.PIPE, check=True, timeout=30)
        expected = json.loads(result.stdout)
        status = "idle"
        if expected is not None:
            playlist = expected["playlist"]
            index = expected["index"]
            status = f"{fingerprint(playlist)} {index} {fingerprint(playlist['items'][index])}"
        if STATE.exists():
            # A previously downgraded device can have a stale native DB with
            # its own WAL. Keep that directory together for recovery.
            backup = Path(tempfile.mkdtemp(prefix="prior-native-", dir=STATE.parent))
            os.rename(STATE, backup / "player")
        STATE.mkdir(mode=0o700)
        os.replace(Path(stage) / "state.sqlite", STATE / "state.sqlite")
        print(status)


if __name__ == "__main__":
    main()
