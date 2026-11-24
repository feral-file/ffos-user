#!/usr/bin/env python3
"""Observe one natural Arrels -> Gazers transition on FF1. Run as root.

Pass a new evidence directory and a trusted copy of playerctl.py. Only the
socket client runs as the socket owner. No playback or recovery commands.
Requires tracefs with uprobes, GNU nm/readelf, and the native FF1 host.
The JSONL stream goes to stdout (the journal under systemd). The evidence
directory holds matching trace snapshots. A run first seeing Gazers already
on screen is marked partial. Probe latency includes client process startup.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
from datetime import datetime

TRACE = Path("/sys/kernel/tracing")
LIB = Path("/usr/lib/ff-player-native/libff_host.so")
SOCKET = Path("/run/user/1000/ff-player/player.sock")
PLAYLIST = "75cba215-484e-4897-ab04-2fec8c44af98"
ARRELS = "0d31f08d-ca86-49ca-90d3-31a10a5ccd34"
GAZERS = "e1c1f34a-aa8f-4138-a1ac-31f9aed1cef9"
RETURNS = "20818270-1c06-4d04-9ea1-315a4e13ee28"

# Main thread boundaries. Also include the lowest-level shared library calls
# so blocked cleanup, GPU fences and display backpressure can be distinguished.
PROBES = {
    LIB: {
        "server": "ff::Server::poll(",
        "tick": "ff::Playback::tick(",
        "events": "ff::Presentation::events(",
        "browser": "ff::PublicBrowser::poll(",
        "browser_destroy": "ff::PublicBrowser::~PublicBrowser(",
        "dispatch": "ff_browser_server_poll",
        "frame": "ff_browser_view_frame",
        "import": "ff::Presentation::accelerated(",
        "destroy": "ff::Presentation::destroy(",
        "draw": "ff::Presentation::draw(",
        "swap": "ff::Presentation::swap(",
        "overlay_init": "ff::CageOverlay::CageOverlay(",
    },
    Path("/usr/lib/libGLESv2.so.2"): {"finish": "glFinish"},
    Path("/usr/lib/libSDL2-2.0.so.0"): {"sdl_swap": "SDL_GL_SwapWindow"},
    Path("/usr/lib/libEGL.so.1"): {"egl_swap": "eglSwapBuffers"},
    Path("/usr/lib/libwayland-client.so.0"): {"roundtrip": "wl_display_roundtrip_queue"},
}


def symbols(lib, wanted):
    headers = subprocess.check_output(["readelf", "-Wl", lib], text=True)
    segments = [tuple(int(f[i], 16) for i in (1, 2, 4)) for line in headers.splitlines()
                if (f := line.split()) and f[0] == "LOAD" and "E" in f[6:-1]]
    names = subprocess.check_output(["nm", "-DC", "--defined-only", lib], text=True)
    found = {}
    for line in names.splitlines():
        parts = line.split(None, 2)
        if len(parts) != 3:
            continue
        addr, _, name = parts
        for label, match in wanted.items():
            if name == match or (match.endswith("(") and name.startswith(match)):
                address = int(addr, 16)
                offset, base, _ = next(s for s in segments if s[1] <= address < s[1] + s[2])
                if label in found and found[label] != offset + address - base:
                    raise ValueError(f"ambiguous symbol {name}")
                found[label] = offset + address - base
    if found.keys() != wanted.keys():
        raise ValueError(f"unresolved symbols in {lib}: {wanted.keys() - found.keys()}")
    return found


def append_control(path, value):
    # tracefs accepts append writes but rejects the seek used by Python's "a" mode.
    fd = os.open(path, os.O_WRONLY | os.O_APPEND)
    try:
        os.write(fd, (value + "\n").encode())
    finally:
        os.close(fd)


class HostTrace:
    def __init__(self, record_swaps=False):
        self.record_swaps = record_swaps
        self.group = f"ff_gazers_{os.getpid()}"
        self.instance = TRACE / "instances" / self.group
        self.registered = []
        self.pid = None

    def start(self, pid):
        self.instance.mkdir()
        (self.instance / "tracing_on").write_text("0")
        (self.instance / "buffer_size_kb").write_text("512" if self.record_swaps else "128")
        (self.instance / "trace_clock").write_text("mono")
        self.set_pid(pid)
        # With a responsive socket there may be no slow-probe snapshots. Retain
        # independent swap entry/return times to compare with completed probes.
        for lib, names in PROBES.items():
            if self.record_swaps and "egl_swap" not in names:
                continue
            for label, offset in symbols(lib.resolve(), names).items():
                for kind in ("p", "r"):
                    event = f"{self.group}/{label}_{kind}"
                    append_control(TRACE / "uprobe_events", f"{kind}:{event} {lib.resolve()}:0x{offset:x}")
                    self.registered.append(event)
        (self.instance / "events" / self.group / "enable").write_text("1")
        (self.instance / "tracing_on").write_text("1")

    def set_pid(self, pid):
        (self.instance / "set_event_pid").write_text(str(pid))
        self.pid = pid

    def save(self, output, tag):
        # Reading trace gives a snapshot without consuming or stopping collection.
        lines = (self.instance / "trace").read_text().splitlines()
        (output / f"{tag}.trace").write_text("\n".join(lines[:10] + lines[-240:]) + "\n")

    def close(self, output):
        try:
            if self.instance.exists():
                (self.instance / "tracing_on").write_text("0")
                if self.record_swaps:
                    (output / "swaps.trace").write_text((self.instance / "trace").read_text())
        finally:
            if self.instance.exists():
                self.instance.rmdir()
            for event in reversed(self.registered):
                append_control(TRACE / "uprobe_events", f"-:{event}")


def host_pid():
    group = Path("/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/chromium-kiosk.service")
    if group.exists():
        for pid in (group / "cgroup.procs").read_text().split():
            try:
                args = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
            except FileNotFoundError:
                continue
            if f"--socket={SOCKET}".encode() in args:
                return int(pid)
    return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    parser.add_argument("playerctl", type=Path)
    parser.add_argument("--wait-seconds", type=int, default=900)
    parser.add_argument("--record-swaps", action="store_true",
                        help="retain host EGL swap times even when every health probe succeeds")
    args = parser.parse_args()
    args.output.mkdir(mode=0o750)
    trace = HostTrace(args.record_swaps)
    owner = SOCKET.stat()

    def emit(event, **data):
        print(json.dumps(dict(time=datetime.now().astimezone().isoformat(timespec="milliseconds"),
                              mono=round(time.monotonic(), 6), event=event, **data)), flush=True)

    def capture(tag):
        trace.save(args.output, tag)
        directory = Path(f"/proc/{trace.pid}")
        sample = {}
        for name in ("stack", "wchan", "syscall", "schedstat"):
            try:
                sample[name] = (directory / name).read_text().strip()
            except OSError as err:
                sample[name] = str(err)
        emit("sample", tag=tag, pid=trace.pid, **sample)

    until = time.monotonic() + args.wait_seconds
    target = None
    previous_presented = None
    control_instance = None
    target_presented = False
    first_timeout = None
    last_target_state = None
    count = 0
    emit("start", boot=Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
         host_sha256=hashlib.sha256(LIB.read_bytes()).hexdigest())
    try:
        while time.monotonic() < until:
            count += 1
            start = time.monotonic()
            if trace.pid and not Path(f"/proc/{trace.pid}").exists():
                replacement = host_pid()
                if replacement:
                    trace.set_pid(replacement)
                    emit("host_changed", pid=replacement)
            with subprocess.Popen([sys.executable, "-I", args.playerctl, "--socket", SOCKET, "health.get"],
                                  user=owner.st_uid, group=owner.st_gid, extra_groups=[],
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE) as client:
                try:
                    out, err = client.communicate(timeout=0.8)
                except subprocess.TimeoutExpired:
                    if trace.pid:
                        capture(f"{count:04d}-slow")
                    try:
                        out, err = client.communicate(timeout=max(0.01, 3 - (time.monotonic() - start)))
                    except subprocess.TimeoutExpired:
                        client.kill()
                        client.communicate()
                        emit("deadline", probe=count, pid=trace.pid, elapsed=round(time.monotonic() - start, 3))
                        if trace.pid:
                            capture(f"{count:04d}-deadline")
                            if first_timeout is None:
                                first_timeout = start
                                until = min(until, start + 60)
                        time.sleep(1)
                        continue
            elapsed = round(time.monotonic() - start, 3)
            if client.returncode:
                emit("probe_error", detail=err.decode(errors="replace")[-350:])
            else:
                state = json.loads(out)
                selected = state.get("selected") or {}
                presented = state.get("presented") or {}
                item = selected.get("item") or {}
                presented_id = (presented.get("item") or {}).get("id")
                media = state.get("media") or {}
                scoped = (state.get("playlist") or {}).get("id") == PLAYLIST
                presented_scoped = (state.get("presentedPlaylist") or {}).get("id") == PLAYLIST
                if presented_scoped and presented_id == ARRELS and control_instance is None:
                    control_instance = state.get("instance")
                emit("health", start_mono=start, elapsed=elapsed, selected=item.get("title"), item=item.get("id"),
                     presented=(presented.get("item") or {}).get("title"), stage=selected.get("stage"),
                     occurrence=selected.get("id"), error=selected.get("error"), renderer=state.get("renderer"),
                     path=media.get("presentationPath"), browser_pid=media.get("browserPid"),
                     exported=media.get("exportedFrames"), direct_frames=media.get("presentedFrames"),
                     media_occurrence=media.get("occurrence"), media_at_ms=media.get("sampledAtMs"),
                     instance=state.get("instance"))
                if scoped and item.get("id") in (ARRELS, GAZERS) and trace.pid is None:
                    pid = host_pid()
                    if pid:
                        trace.start(pid)
                        emit("armed", pid=pid, events=len(trace.registered))
                elif trace.pid and not (args.output / "baseline.trace").exists():
                    capture("baseline")
                if scoped and item.get("id") == GAZERS and target is None:
                    target = start
                    until = min(until, target + 310)
                    observed_control = previous_presented == ARRELS or presented_id == ARRELS
                    emit("target", coverage="transition" if observed_control else "partial")
                if not scoped or presented_id:
                    previous_presented = presented_id if scoped else None
                if target and presented_scoped and presented_id == GAZERS:
                    target_presented = True
                if scoped and item.get("id") == GAZERS and trace.pid:
                    phase = (selected.get("id"), selected.get("stage"), media.get("presentationPath"))
                    if phase != last_target_state:
                        capture(f"{count:04d}-target")
                        last_target_state = phase
                if target and presented_id == item.get("id") and presented_id not in (None, ARRELS, GAZERS):
                    if trace.pid:
                        capture("departure")
                    reason = "successor_presented"
                    if not target_presented:
                        reason = "target_never_presented"
                    elif not scoped or not presented_scoped or presented_id != RETURNS:
                        reason = "unexpected_successor"
                    elif control_instance is None or control_instance != state.get("instance"):
                        reason = "incomplete_or_restarted_host"
                    emit("finished", reason=reason, target_presented=target_presented,
                         control_instance=control_instance, final_instance=state.get("instance"))
                    return
            time.sleep(1 if trace.pid else 4)
        emit("finished", reason="post_timeout_window" if first_timeout else "target_window" if target else "target_unseen",
             target_observed=target is not None)
    finally:
        trace.close(args.output)


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    main()
