#!/usr/bin/env python3
"""Loopback-only media fixtures with byte ranges; not a production cache service."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import mimetypes
from pathlib import Path
import re
import sys
from urllib.parse import unquote, urlsplit

root = Path(sys.argv[1]).resolve()
class Handler(BaseHTTPRequestHandler):
    def do_HEAD(self):
        self.media(False)
    def do_GET(self):
        self.media(True)
    def media(self, body):
        path = (root / unquote(urlsplit(self.path).path).lstrip('/')).resolve()
        if not path.is_relative_to(root) or not path.is_file():
            self.send_error(404)
            return
        size = path.stat().st_size
        start, end = 0, size - 1
        status = 200
        requested = self.headers.get('Range')
        if requested:
            match = re.fullmatch(r'bytes=(\d+)-(\d*)', requested)
            if not match or int(match[1]) >= size:
                self.send_response(416)
                self.send_header('Content-Range', f'bytes */{size}')
                self.send_header('Content-Length', '0')
                self.end_headers()
                return
            start = int(match[1])
            end = min(size - 1, int(match[2]) if match[2] else size - 1)
            if end < start:
                self.send_error(416)
                return
            status = 206
        self.send_response(status)
        self.send_header('Content-Type', mimetypes.guess_type(path.name)[0] or 'application/octet-stream')
        self.send_header('Content-Length', str(end - start + 1))
        self.send_header('Accept-Ranges', 'bytes')
        # Models intentionally load from the separate bundled renderer origin.
        # This is fixture-server policy, never a header rewrite for artist URLs.
        self.send_header('Access-Control-Allow-Origin', '*')
        if status == 206:
            self.send_header('Content-Range', f'bytes {start}-{end}/{size}')
        self.end_headers()
        if body:
            with path.open('rb') as stream:
                stream.seek(start)
                remaining = end - start + 1
                try:
                    while remaining:
                        data = stream.read(min(65536, remaining))
                        if not data:
                            break
                        self.wfile.write(data)
                        remaining -= len(data)
                except (BrokenPipeError, ConnectionResetError):
                    pass
ThreadingHTTPServer(('127.0.0.1', 18990), Handler).serve_forever()
