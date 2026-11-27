#!/usr/bin/env python3
import json, os, socket, struct, sys, time
import hashlib
from pathlib import Path


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def check():
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
        if "--presented" in sys.argv:
            shown = state.get("presented") or {}
            playlist_hash, index, item_hash = Path(sys.argv[2]).read_text().split()
            if not (state.get("presentedOwnsCast") and shown.get("stage") == "presented"
                    and fingerprint(state.get("presentedPlaylist")) == playlist_hash):
                sys.exit("Waiting for the migrated playlist to present")
            if shown["index"] == int(index) and fingerprint(shown["item"]) == item_hash:
                return
            # Short slots may advance before the service's first poll. The
            # migration creates a fresh history and only real frames enter it.
            history = call("history.list", {})
            first = history["records"][-1] if history["records"] else {}
            playlist_id = state["presentedPlaylist"].get("id")
            if not (not history["incomplete"] and playlist_id and first.get("playlistId") == playlist_id
                    and first.get("index") == int(index) and fingerprint(first.get("item")) == item_hash):
                sys.exit("Waiting for evidence of the migrated artwork")



if __name__ == "__main__":
    try:
        check()
    except Exception as error:
        sys.exit(f"Player health check failed: {error}")
