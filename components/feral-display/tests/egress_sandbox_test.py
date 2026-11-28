"""A real worker namespace, an owned host canary, and no external traffic."""
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys

helper = str(Path(sys.argv[1]).resolve())
bwrap = shutil.which("bwrap")
curl = shutil.which("curl")
if not bwrap or not curl:
    sys.exit(77)
sandbox = [bwrap, "--die-with-parent", "--unshare-all", "--ro-bind", "/", "/",
           "--proc", "/proc", "--dev", "/dev"]
if subprocess.run([*sandbox, "/bin/true"]).returncode:
    sys.exit(77)  # Some CI containers disable nested user namespaces.

with socket.socket() as canary:
    canary.bind(("127.0.0.1", 0))
    canary.listen()
    host = f"127.0.0.1:{canary.getsockname()[1]}"
    for scheme in ("http", "https"):
        cmd = [helper, "host", *sandbox, helper, "inside", curl, "--max-time", "4",
               "--silent", "--output", "/dev/null", "--noproxy", "", "--proxy",
               "http://127.0.0.1:17873", "--write-out", "%{http_code} %{http_connect}",
               f"{scheme}://{host}/canary"]
        child = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                 text=True, start_new_session=True)
        try:
            out, err = child.communicate(timeout=8)
        except subprocess.TimeoutExpired:
            os.killpg(child.pid, signal.SIGKILL)
            raise AssertionError("worker exit left the namespace or proxy alive")
        assert out == ("502 000" if scheme == "http" else "000 502"), (out, err)
    canary.setblocking(False)
    try:
        connection, _ = canary.accept()
    except BlockingIOError:
        print("HTTP and CONNECT denied. Host canary received zero connections. Both workers exited.")
    else:
        connection.close()
        raise AssertionError("host canary received a connection")
