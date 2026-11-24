#!/usr/bin/env python3
"""Run on Linux with a disposable Cage display and the installed native runtime.

  python3 tools/test-artwork-stall.py /usr/lib/ff-player-native/ff-player-native --display wayland-test

Uses a private host, socket, state and inline artwork. Run once per presentation
path. Requires pidfds and visibility of the test host's own sandbox descendants.
"""
import argparse
import base64
from contextlib import ExitStack
import os
from pathlib import Path
import signal
import socket
import subprocess
from tempfile import TemporaryDirectory
import time
import uuid

from playerctl import exchange


def call(path, method, params):
    start = time.monotonic()
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(3)
        conn.connect(str(path))
        hello = exchange(conn, {"v": 1, "id": "hello", "method": "hello",
                                "params": {"client": "stall-test", "versions": [1]}})
        remaining = 3 - (time.monotonic() - start)
        if remaining <= 0:
            raise TimeoutError("hello used the watchdog deadline")
        conn.settimeout(remaining)
        state = exchange(conn, {"v": 1, "id": "probe", "method": method, "params": params,
                                "instance": hello["instance"], "generation": hello["snapshot"]["generation"],
                                "key": str(uuid.uuid4()), "deadlineMs": int((time.time() + remaining) * 1000)})
        if method == "health.get" and state.get("instance") != hello["instance"]:
            raise AssertionError("Health reply came from a different host instance")
    if time.monotonic() - start >= 3:
        raise TimeoutError(f"hello + {method} exceeded the watchdog deadline")
    return state


def pause_renderers(host_pid, launcher, stack):
    # Traverse only this host's worker. Pin each renderer before checking its
    # role so a PID recycled during teardown cannot receive a later signal.
    def children(pid):
        for task in Path(f"/proc/{pid}/task").glob("*/children"):
            try:
                yield from map(int, task.read_text().split())
            except FileNotFoundError:
                continue

    if launcher not in children(host_pid):
        raise RuntimeError("Reported browser is outside the test host")

    def is_test_renderer(pid):
        argv = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
        # Chromium's zygote may rewrite argv[0] as a space-separated process title.
        renderer_title = argv[0].split()[:2] == [os.fsencode(Path(f"/proc/{host_pid}/exe").readlink()), b"--type=renderer"]
        if b"--type=renderer" not in argv and not renderer_title:
            return False
        while pid > 1:
            status = Path(f"/proc/{pid}/status").read_text().splitlines()
            parent = int(next(line.split()[1] for line in status if line.startswith("PPid:")))
            if pid == launcher:
                return parent == host_pid
            pid = parent
        return False

    pending = [launcher]
    paused = 0
    while pending:
        pid = pending.pop()
        try:
            fd = os.pidfd_open(pid)
        except ProcessLookupError:
            continue
        try:
            pending.extend(children(pid))
            if is_test_renderer(pid):
                signal.pidfd_send_signal(fd, signal.SIGSTOP)
                stack.callback(resume_and_close, fd)
                fd = None
                paused += 1
        except (ProcessLookupError, FileNotFoundError):
            continue
        finally:
            if fd is not None:
                os.close(fd)
    if not paused:
        raise RuntimeError("No renderer in the selected test worker")


def resume_and_close(fd):
    try:
        signal.pidfd_send_signal(fd, signal.SIGCONT)
    except ProcessLookupError:
        pass
    finally:
        os.close(fd)


def check(host, endpoint, direct):
    deadline = time.monotonic() + 90
    last_error = None
    while True:
        if host.poll() is not None or time.monotonic() > deadline:
            raise RuntimeError("Test host did not finish starting") from last_error
        try:
            instance = call(endpoint, "health.get", {})["instance"]
            break
        except (OSError, EOFError, RuntimeError) as error:
            # The listener exists before graphics initialization. A full
            # backlog or an expired request is transient during that window.
            last_error = error
            time.sleep(.1)
    call(endpoint, "connectivity.set", {"online": False})
    items = []
    for name, color in [("stalls", "blue"), ("successor", "green")]:
        # A direct -> composited fallback retains the occurrence ID and can
        # export about:blank frames. Identify a load from this document in the
        # new browser before suspending it.
        html = (f"<body style='background:{color}'>{name}</body>"
                "<script>onload=()=>requestAnimationFrame(()=>history.replaceState(null,'','#ready'))</script>").encode()
        items.append({"id": name, "title": name, "duration": 0,
                      "source": "data:text/html;base64," + base64.b64encode(html).decode(),
                      "display": {"margin": 0 if direct else 32}})
    receipt = call(endpoint, "playback.replace", {"playlist": {"items": items}})
    paused_workers = set()
    nested_attempts = {}
    direct_occurrences = set()
    failures = set()
    waiting = None
    # Allow direct -> composited fallback and a five-second health cycle on
    # each of four bounded attempts. Every probe has its own three-second SLA.
    deadline = time.monotonic() + 180
    with ExitStack() as stopped:
        while time.monotonic() < deadline:
            state = call(endpoint, "health.get", {})
            if (state["instance"] != instance or state["host"] != "responsive"
                    or state["generation"] != receipt["generation"]):
                raise AssertionError("Host instance, generation or responsiveness changed")
            selected = state["selected"] or {}
            visible = state["presented"] or {}
            media = state["media"] or {}
            recovery = state["recovery"]
            if recovery["failedOccurrence"] and "layout health probe" in recovery["lastError"]:
                failures.add(recovery["failedOccurrence"])
            if waiting is not None:
                occurrence, previous_pid, path, since = waiting
                # Worst case: one cadence before enqueue, one reply deadline,
                # then the one-second media sample and socket/scheduling jitter.
                if time.monotonic() - since > 15:
                    raise AssertionError(f"{path} did not recover within 15s of pausing {previous_pid}")
                if (selected.get("id") != occurrence or occurrence in failures or
                        (path == "cage" and media.get("presentationPath") == "composited"
                         and media.get("browserPid") != previous_pid)):
                    waiting = None
            if visible.get("item", {}).get("id") == "successor":
                # The final failure can advance the index before the next
                # socket poll. The first three errors survive into each retry.
                if (set(nested_attempts.values()) != {0, 1, 2, 3} or len(nested_attempts) != 4
                        or any(k not in failures for k, v in nested_attempts.items() if v < 3)
                        or (direct and receipt["occurrence"] not in direct_occurrences)
                        or (not direct and direct_occurrences)):
                    raise AssertionError(f"Unexpected recovery: {failures}, {nested_attempts}, {direct_occurrences}")
                return
            pid = media.get("browserPid")
            if (selected.get("id") and selected["id"] == visible.get("id")
                    and selected["item"].get("id") == "stalls" and pid and pid not in paused_workers
                    and media.get("occurrence") == selected["id"]
                    and media.get("documentUrl") == selected["item"]["source"] + "#ready"
                    and (media.get("presentedFrames") or media.get("exportedFrames"))):
                paused_workers.add(pid)
                pause_renderers(host.pid, pid, stopped)
                waiting = (selected["id"], pid, media["presentationPath"], time.monotonic())
                if media["presentationPath"] == "cage":
                    direct_occurrences.add(selected["id"])
                else:
                    nested_attempts[selected["id"]] = recovery["attempt"]
            time.sleep(.1)
    raise AssertionError(f"No successor after layout failures {failures}, attempts {nested_attempts}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("--display", required=True, help="disposable Cage display")
    parser.add_argument("--path", choices=("composited", "cage"), default="composited")
    args = parser.parse_args()
    with TemporaryDirectory(prefix="ff-stall-") as tmp:
        root = Path(tmp)
        endpoint = root / "player.sock"
        env = {**os.environ, "HOME": tmp, "XDG_CACHE_HOME": str(root / "cache"),
               "SDL_VIDEODRIVER": "wayland", "WAYLAND_DISPLAY": args.display}
        host = subprocess.Popen([args.binary.resolve(), f"--state-dir={root / 'state'}",
                                 f"--socket={endpoint}"], env=env)
        try:
            check(host, endpoint, args.path == "cage")
            print(f"{args.path}: timely host health replies through repeated artwork stalls and skip")
        finally:
            host.terminate()
            try:
                host.wait(timeout=5)
            except subprocess.TimeoutExpired:
                host.kill()
                host.wait()


if __name__ == "__main__":
    main()
