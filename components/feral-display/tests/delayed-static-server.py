#!/usr/bin/env python3
"""Device fixture: final visible paint precedes a nonvisual load dependency."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import time


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/late.js':
            time.sleep(3)
            body, mime = b'/* delayed, nonvisual */', 'text/javascript'
        elif self.path == '/favicon.ico':
            self.send_error(404)
            return
        else:
            body = b'<!doctype html><body style="margin:0;background:#267d46"><script async src="/late.js"></script>'
            mime = 'text/html'
        self.send_response(200)
        self.send_header('Content-Type', mime)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


ThreadingHTTPServer(('127.0.0.1', 18991), Handler).serve_forever()
