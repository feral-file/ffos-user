#!/usr/bin/env python3
"""Observe a running signed playlist on FF1. Does not change playback or settings."""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import time
import threading
import struct
import zlib
from contextlib import ExitStack
from datetime import datetime, timezone

from playerctl import exchange


def utc():
    return datetime.now(timezone.utc).isoformat()


def player_state(path):
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(3)
        conn.connect(path)
        return exchange(conn, {"v": 1, "id": "hello", "method": "hello",
                               "params": {"client": "playlist-soak", "versions": [1]}})["snapshot"]


def append(path, data):
    with path.open("a") as log:
        print(json.dumps(data, separators=(",", ":")), file=log, flush=True)
        os.fsync(log.fileno())


def public_text(value):
    # Headers, LAN control envelopes and signed URL parameters do not belong in a soak report.
    return re.sub(r"(https?://[^\s?#]+)[?#][^\s]*", r"\1?<redacted>", str(value))[:700]


def process_stats(pid):
    if not isinstance(pid, int) or pid <= 0:
        return None
    try:
        fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
        return {"pid": pid, "startTicks": int(fields[19]),
                "cpuTicks": int(fields[11]) + int(fields[12]),
                "rssPages": int(fields[21])}
    except (OSError, IndexError, ValueError):
        return None


def screen(directory, seq):
    # One capture yields both the evidence and the measurement. A uniform frame
    # is a review candidate: minimal and intentionally blank artworks exist.
    data = subprocess.run(["grim", "-s", "0.15", "-t", "ppm", "-"], capture_output=True,
                          timeout=5, check=True).stdout
    header = re.match(rb"P6\s+(\d+)\s+(\d+)\s+255\s", data)
    if not header:
        raise ValueError("Unexpected screenshot format")
    w, h = (int(v) for v in header.groups())
    pixels = data[header.end():]
    if len(pixels) != w * h * 3:
        raise ValueError("Incomplete screenshot")
    def chunk(kind, body):
        return (struct.pack(">I", len(body)) + kind + body +
                struct.pack(">I", zlib.crc32(kind + body)))

    # Store a small lossless PNG, even on an image without Pillow.
    rows = b"".join(b"\0" + pixels[y*w*3:(y+1)*w*3] for y in range(h))
    image = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)) +
             chunk(b"IDAT", zlib.compress(rows, 3)) + chunk(b"IEND", b""))
    name = f"frames/{seq:07d}.png"
    (directory/name).write_bytes(image)
    bands = [pixels[c::3] for c in range(3)]
    deviation = [math.sqrt(max(0, sum(x*x for x in band)/len(band) -
                                   (sum(band)/len(band))**2)) for band in bands]
    return {"file": name, "pixelSha256": hashlib.sha256(pixels).hexdigest(),
            "maxChannelStdDev": round(max(deviation), 2)}


def collect_logs(stack, root):
    # Service units can write to their own rotated files. Keep the journal
    # (including systemd restart messages) as well as the application errors.
    commands = {
        "units": ["journalctl", "--user", "-n0", "-f", "-o", "cat", "-u", "chromium-kiosk",
                  "-u", "feral-controld", "-u", "feral-watchdog"],
        "kernel": ["journalctl", "-k", "-n0", "-f", "-o", "cat", "-g",
                   "(?i)drm|gpu|watchdog|thermal|out of memory|oom|hardware error"],
    }
    for name in ("chromium", "controld", "watchdog"):
        log = Path.home()/".logs"/(name+".log")
        if log.is_file():
            commands[name] = ["tail", "-n0", "-F", str(log)]

    def copy(name, proc):
        count = skipped = 0
        path = root/(name+".jsonl")
        for line in proc.stdout:
            line = line.strip()
            if name in ("controld", "watchdog"):
                try:
                    obj = json.loads(line)
                except ValueError:
                    continue
                if obj.get("level", "").lower() not in ("warn","warning","error","fatal","panic"):
                    continue
                line = " ".join(str(obj[k]) for k in ("timestamp","level","msg","message","error") if obj.get(k))
            if not line:
                continue
            if count >= 100000:
                skipped += 1
                continue
            append(path, {"utc":utc(),"message":public_text(line)})
            count += 1
        append(root/"log-streams.jsonl", {"utc":utc(),"stream":name,"lines":count,"skipped":skipped})

    def stop(proc, thread):
        proc.terminate()
        try:
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
        thread.join(timeout=3)
        proc.stdout.close()

    for name, command in commands.items():
        proc = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, errors="replace")
        thread = threading.Thread(target=copy, args=(name,proc), daemon=True)
        thread.start()
        stack.callback(stop, proc, thread)

def observe(args, stack):
    root = Path(args.output)
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    (root/"frames").mkdir()
    collect_logs(stack, root)
    start = time.monotonic()
    boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    append(root/"run.jsonl", {"event": "start", "utc": utc(), "boot": boot,
           "playlist": args.playlist, "seconds": args.seconds, "interval": args.interval,
           "kernel": os.uname().release, "clockTicks": os.sysconf("SC_CLK_TCK"),
           "pageBytes": os.sysconf("SC_PAGE_SIZE")})
    previous = None
    samples = errors = seq = 0
    next_tick = start
    last_frame = -1000
    transition = start
    second_frame = False
    while time.monotonic()-start < args.seconds:
        t = time.monotonic()
        event = {"utc": utc(), "elapsed": round(t-start, 3), "seq": seq, "boot": boot}
        try:
            s = player_state(args.socket)
            current = s.get("selected") or {}
            shown = s.get("presented") or {}
            item = current.get("item") or {}
            media = s.get("media") or {}
            pl = s.get("playlist") or {}
            failure = (current.get("error") or s.get("presentationFailure") or
                       (s.get("recovery") or {}).get("lastError"))
            overlay = (s.get("overlay") or {}).get("state")
            event.update(instance=s.get("instance"), playlist=pl.get("id"),
                         matches=pl.get("id")==args.playlist, index=current.get("index"),
                         title=item.get("title"), itemId=item.get("id"),
                         source=public_text(item.get("source","")),
                         selected=current.get("id"), shown=shown.get("id"),
                         stage=current.get("stage"), overlay=overlay, online=s.get("online"),
                         error=public_text(failure) if failure else None,
                         renderer=media.get("backend"), path=media.get("presentationPath"),
                         document=public_text(media.get("documentUrl","")),
                         process=process_stats(media.get("browserPid") or media.get("workerPid")),
                         counters={k:media[k] for k in ("presentedFrames","exportedFrames",
                                   "importedFrames","frame-drop-count","decoder-frame-drop-count",
                                   "cacheHits","cacheMisses","time-pos","offline",
                                   "inlineScriptBlocked","startupReferenceError") if k in media})
            key = (event["instance"], event["selected"], event["shown"], event["stage"],
                   overlay, event["error"], event["matches"])
            if key != previous:
                transition = t
                second_frame = False
            settled = not second_frame and t-transition >= args.settle
            if key != previous or settled or t-last_frame >= 60:
                if shutil.disk_usage(root).free < 128*1024*1024:
                    raise OSError("Low disk space for screen capture")
                event["screen"] = screen(root, seq)
                last_frame = t
                if settled:
                    second_frame = True
            previous = key
            samples += 1
        except (OSError, ValueError, RuntimeError, EOFError, subprocess.SubprocessError) as e:
            errors += 1
            event["observerError"] = public_text(e)
        append(root/"observations.jsonl", event)
        if seq % 15 == 0:
            memory = Path("/proc/meminfo").read_text().splitlines()
            append(root/"health.jsonl", {"utc": utc(), "seq": seq,
                   "memAvailableKiB": int(next(s.split()[1] for s in memory if s.startswith("MemAvailable:"))),
                   "load": Path("/proc/loadavg").read_text().strip(),
                   "diskFree": shutil.disk_usage(root).free})
        seq += 1
        next_tick = max(next_tick + args.interval, time.monotonic())
        time.sleep(max(0, min(next_tick,start+args.seconds)-time.monotonic()))
    append(root/"run.jsonl", {"event":"complete","utc":utc(),"seconds":time.monotonic()-start,
                              "samples":samples,"observerErrors":errors})


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", required=True)
    parser.add_argument("--playlist", required=True)
    parser.add_argument("--output", required=True, help="A new evidence directory")
    parser.add_argument("--seconds", type=int, required=True)
    parser.add_argument("--interval", type=float, default=2)
    parser.add_argument("--settle", type=float, default=5)
    args = parser.parse_args()
    if args.seconds < 1 or args.interval < .25 or args.settle < 0:
        parser.error("Invalid duration or sampling interval")
    with ExitStack() as stack:
        observe(args, stack)
