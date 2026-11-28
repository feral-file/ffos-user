#!/usr/bin/env python3
"""Operator-only, ephemeral loopback bridge into a test browser's network namespace.

Run over SSH with stdin kept open. EOF or120 seconds closes the bridge. This
never mounts a host control socket inside artwork or changes browser isolation.
"""
import os,sys,socket,select,threading,time
from pathlib import Path
launcher,port=map(int,sys.argv[1:]);assert launcher>1 and 0<port<65536
pending=[launcher];browser=None
for _ in range(128):
 if not pending:break
 pid=pending.pop()
 try:
  args=Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')
  if args[0].endswith(b'/feral-display') and b'--cef' in args and not any(x.startswith(b'--type=') for x in args):browser=pid;break
  pending.extend(map(int,Path(f'/proc/{pid}/task/{pid}/children').read_text().split()))
 except(FileNotFoundError,ProcessLookupError):pass
assert browser,'Test browser descendant absent'
listener=socket.socket();listener.bind(('127.0.0.1',0));listener.listen(8)
namespace=os.open(f'/proc/{browser}/ns/net',os.O_RDONLY);os.setns(namespace,0);os.close(namespace)
print(listener.getsockname()[1],flush=True)
slots=threading.BoundedSemaphore(8)
def relay(client):
 try:
  with client,socket.create_connection(('127.0.0.1',port),timeout=5) as target:
   while True:
    readable,_,_=select.select([client,target],[],[],5)
    if not readable:continue
    for source in readable:
     data=source.recv(16384)
     if not data:return
     (target if source is client else client).sendall(data)
 except(OSError,TimeoutError):pass
 finally:slots.release()
until=time.monotonic()+120
while time.monotonic()<until:
 ready,_,_=select.select([listener,sys.stdin],[],[],1)
 if sys.stdin in ready and not os.read(0,1):break
 if listener in ready:
  client,_=listener.accept()
  if slots.acquire(False):threading.Thread(target=relay,args=(client,),daemon=True).start()
  else:client.close()
listener.close()
