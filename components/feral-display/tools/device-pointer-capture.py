#!/usr/bin/env python3
"""Diagnostic Cage capture after native input; not proof of panel output."""
import argparse
import socket
import subprocess
import time
import uuid
from playerctl import exchange

parser = argparse.ArgumentParser()
parser.add_argument('--socket', required=True)
parser.add_argument('--output', required=True)
args = parser.parse_args()
with socket.socket(socket.AF_UNIX) as connection:
    connection.settimeout(5)
    connection.connect(args.socket)
    hello = exchange(connection, {'v':1, 'id':'hello', 'method':'hello',
                                  'params':{'client':'pointer-capture', 'versions':[1]}})
    state = hello['snapshot']
    assert state['presented']['item']['source'].endswith('/input.html')
    exchange(connection, {'v':1, 'id':'move', 'method':'input.dispatch',
                          'params':{'kind':'move','occurrence':state['presented']['id'],'x':400,'y':500},
                          'instance':state['instance'],'generation':state['generation'],
                          'key':str(uuid.uuid4()),'deadlineMs':int(time.time()*1000)+4000})
    time.sleep(0.2)
    subprocess.run(['grim', args.output], check=True, timeout=10)
