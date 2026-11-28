#!/usr/bin/env python3
"""FFOS GPU/decoder diagnostic. Buffer export is not display-output evidence."""
import argparse
import array
import json
import os
import signal
import socket
import time

parser = argparse.ArgumentParser()
parser.add_argument('worker')
parser.add_argument('source')
parser.add_argument('--width', type=int, default=1920)
parser.add_argument('--height', type=int, default=1080)
parser.add_argument('--seconds', type=float, default=8)
args = parser.parse_args()
parent, child = socket.socketpair(socket.AF_UNIX, socket.SOCK_SEQPACKET)
actions = [(os.POSIX_SPAWN_CLOSE, parent.fileno()), (os.POSIX_SPAWN_DUP2, child.fileno(), 3)]
if child.fileno() != 3:
    actions.append((os.POSIX_SPAWN_CLOSE, child.fileno()))
pid = os.posix_spawn(args.worker, [args.worker], os.environ, file_actions=actions)
child.close()
parent.settimeout(1)
def send(value):
    parent.send(json.dumps(value).encode())
started = time.monotonic()
frames = 0
properties = {}
loaded = False
layout = None
try:
    send({'method':'initialize','source':args.source,'width':args.width,'height':args.height,
          'display':{'loop':True,'scaling':'fit'},'paused':False,'volume':0,'muted':True})
    while time.monotonic() - started < args.seconds:
        try:
            data, ancillary, flags, _ = parent.recvmsg(65536, socket.CMSG_SPACE(4 * array.array('i').itemsize))
        except TimeoutError:
            continue
        if not data:
            raise RuntimeError('Worker disconnected')
        fds = array.array('i')
        for level, kind, payload in ancillary:
            if level == socket.SOL_SOCKET and kind == socket.SCM_RIGHTS:
                fds.frombytes(payload)
        try:
            if flags & (socket.MSG_TRUNC | socket.MSG_CTRUNC):
                raise RuntimeError('Truncated worker packet')
            event = json.loads(data)
            if event['event'] == 'error':
                raise RuntimeError(event['message'])
            if event['event'] == 'loaded':
                loaded = True
            if event['event'] == 'property':
                properties[event['name']] = event['value']
            if event['event'] == 'frame':
                if len(fds) != len(event['planes']):
                    raise RuntimeError('Missing DMA-BUF descriptors')
                for fd in fds:
                    os.fstat(fd)
                frames += 1
                layout = event
                send({'method':'ack','sequence':event['sequence']})
        finally:
            for fd in fds:
                os.close(fd)
    if not loaded or frames < 2:
        raise RuntimeError('No decoded GPU frames')
    print(json.dumps({'evidence':'decoder and GPU export only; not imported or displayed',
                      'frames':frames,'elapsedSeconds':time.monotonic()-started,
                      'properties':properties,'lastFrame':layout}), flush=True)
finally:
    try:
        send({'method':'stop'})
    except OSError:
        pass
    parent.close()
    until = time.monotonic() + 3
    while time.monotonic() < until:
        ended, _ = os.waitpid(pid, os.WNOHANG)
        if ended:
            break
        time.sleep(0.05)
    else:
        os.kill(pid, signal.SIGKILL)
        os.waitpid(pid, 0)
