#!/usr/bin/env python3
"""SIGKILL verified renderer PIDs belonging only to the isolated FF1 test unit.

PID roles come from that browser's SystemInfo.getProcessInfo diagnostic. The
unit main PID and cgroup membership are independently checked on the device.
Pidfds bind the kill to a process lifetime, preventing PID-reuse misdirection.
"""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys

unit, browser, *renderer_args = sys.argv[1:]
if unit != 'ff-native-runtime-test.service':
    raise ValueError('Only the isolated native test unit is allowed')
main = int(browser)
renderers = [int(value) for value in renderer_args]
if not renderers or len(renderers) > 8 or main in renderers or len(set(renderers)) != len(renderers):
    raise ValueError('Invalid isolated renderer PID set')
props = dict(line.split('=', 1) for line in subprocess.check_output(
    ['systemctl', '--user', 'show', unit, '-p', 'MainPID', '-p', 'ControlGroup'], text=True).splitlines())
if main != int(props['MainPID']) or not props['ControlGroup'].endswith('/'+unit):
    raise ValueError('Diagnostic browser is not the test unit main process')
cgroup = Path('/sys/fs/cgroup'+props['ControlGroup'])
fds = []
try:
    for pid in renderers:
        fd = os.pidfd_open(pid)
        fds.append(fd)
        members = {int(p) for p in (cgroup/'cgroup.procs').read_text().split()}
        if pid not in members or pid == main:
            raise ValueError('Renderer is outside the isolated test cgroup')
    for fd in fds:
        signal.pidfd_send_signal(fd, signal.SIGKILL)
    print(json.dumps({'unit':unit,'browser':main,'renderers':renderers,'signal':'SIGKILL'}))
finally:
    for fd in fds:
        os.close(fd)
