#!/usr/bin/env python3
"""Small protocol reference client, Python standard library only."""
import argparse
import json
import socket
import struct
import time
import uuid

MAX_FRAME = 4 * 1024 * 1024


def exact(connection, size):
    result = bytearray()
    while len(result) < size:
        data = connection.recv(size - len(result))
        if not data:
            raise EOFError("player disconnected; command outcome may be unknown")
        result.extend(data)
    return bytes(result)


def receive(connection):
    size, = struct.unpack("!I", exact(connection, 4))
    if not 0 < size <= MAX_FRAME:
        raise ValueError("invalid frame length")
    return json.loads(exact(connection, size))


def exchange(connection, request):
    body = json.dumps(request, separators=(",", ":")).encode()
    if not 0 < len(body) <= MAX_FRAME:
        raise ValueError("request exceeds frame limit")
    connection.sendall(struct.pack("!I", len(body)) + body)
    response = receive(connection)
    if response.get("id") != request["id"]:
        raise ValueError("response correlation mismatch")
    if "error" in response:
        raise RuntimeError(json.dumps(response["error"]))
    return response["result"]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--socket", required=True)
    parser.add_argument("--watch", type=float, default=0)
    parser.add_argument("method", default="state.get", nargs="?")
    parser.add_argument("params", default="{}", nargs="?")
    args = parser.parse_args()
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(5)
        connection.connect(args.socket)
        hello = exchange(connection, {"v": 1, "id": "hello", "method": "hello", "params": {"client": "playerctl", "versions": [1]}})
        request = {"v": 1, "id": "command", "method": args.method, "params": json.loads(args.params),
                   "instance": hello["instance"], "generation": hello["snapshot"]["generation"],
                   "key": str(uuid.uuid4()), "deadlineMs": int(time.time()*1000) + 5000}
        print(json.dumps(exchange(connection, request)), flush=True)
        if args.watch:
            request.update(id="subscribe", method="state.subscribe", params={})
            print(json.dumps(exchange(connection, request)), flush=True)
            until = time.monotonic() + args.watch
            while time.monotonic() < until:
                connection.settimeout(until - time.monotonic())
                try:
                    print(json.dumps(receive(connection)), flush=True)
                except TimeoutError:
                    break


if __name__ == "__main__":
    main()
