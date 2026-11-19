#!/usr/bin/env python3
"""Isolated hardware fixture: valid cache metadata followed by an evicted file."""
from http.server import BaseHTTPRequestHandler
from socketserver import UnixStreamServer
import json,struct,sys
class Handler(BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_POST(self):
  request=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
  if self.path!='/v1/policy':self.send_error(404);return
  mode='isolated'if'cache-isolated'in request['source']else'mixed'
  metadata={'version':1,'status':200,'headers':[],'size':0,'sha256':'','complete':False,'requestUrl':request['url'],'replayMode':mode}
  data=json.dumps(metadata).encode();body=struct.pack('>I',len(data))+data
  self.send_response(200);self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
UnixStreamServer(sys.argv[1],Handler).serve_forever()
