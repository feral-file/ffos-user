#!/bin/bash
# Readiness is observation only. systemd/watchdog own process recovery.
for attempt in $(seq 1 90); do
    if python3 - <<'PY'
import json, os, socket, struct, time
try:
    with socket.socket(socket.AF_UNIX) as s:
        s.settimeout(1)
        s.connect(os.environ.get('FF_PLAYER_SOCKET', '/run/user/1000/ff-player/player.sock'))
        assert struct.unpack('3i', s.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))[1] == os.geteuid()
        def read(n):
            data = bytearray()
            while len(data) < n:
                chunk = s.recv(n - len(data))
                if not chunk: raise EOFError()
                data.extend(chunk)
            return data
        def call(method, params):
            request = dict(v=1, id=method, method=method, params=params)
            if method != 'hello': request['deadlineMs'] = int(time.time() * 1000) + 1000
            body = json.dumps(request).encode()
            s.sendall(struct.pack('!I', len(body)) + body)
            n, = struct.unpack('!I', read(4))
            assert 0 < n <= 4194304
            reply = json.loads(read(n))
            assert reply['v'] == 1 and reply['id'] == method and 'error' not in reply
            return reply['result']
        hello = call('hello', dict(client='readiness', versions=[1]))
        state = call('health.get', {})
        assert hello['version'] == 1 and state['instance'] == hello['instance']
        assert state['host'] == 'responsive' and state['generation'] > 0 and state['revision'] > 0
except Exception:
    raise SystemExit(1)
PY
    then
        systemctl --user start chromium-ready.target
        exit 0
    fi
    sleep 1
done
echo 'Native player readiness timed out; watchdog owns recovery' >&2
exit 1
