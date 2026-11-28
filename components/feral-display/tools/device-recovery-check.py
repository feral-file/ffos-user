#!/usr/bin/env python3
"""Inject decoder failures into an explicitly selected isolated test runtime."""
import argparse
import json
import os
import signal
import socket
import time
import uuid
from playerctl import exchange

parser=argparse.ArgumentParser()
parser.add_argument('--socket',required=True)
parser.add_argument('--output',required=True)
args=parser.parse_args()
connection=socket.socket(socket.AF_UNIX)
connection.settimeout(5)
connection.connect(args.socket)
evidence={'checks':[],'startedAt':time.time()}

def call(method,params=None):
    hello=exchange(connection,{'v':1,'id':str(uuid.uuid4()),'method':'hello','params':{'client':'recovery-check','versions':[1]}})
    return exchange(connection,{'v':1,'id':str(uuid.uuid4()),'method':method,'params':params or {},
        'instance':hello['instance'],'generation':hello['snapshot']['generation'],
        'deadlineMs':int(time.time()*1000)+4000,'key':str(uuid.uuid4())})

def wait(predicate,timeout=20):
    deadline=time.monotonic()+timeout
    while time.monotonic()<deadline:
        state=call('state.get')
        if predicate(state): return state
        time.sleep(.05)
    raise TimeoutError(json.dumps(state))

try:
    receipt=call('playback.replace',{'playlist':{'items':[{'source':'http://127.0.0.1:18990/media-4k-h264.mp4','duration':0}]}})
    state=wait(lambda s:s['renderer']=='presented' and (s['media'] or {}).get('workerPid'))
    instance=state['instance'];generation=state['generation']
    assert state['selected']['id']==receipt['occurrence']
    for attempt in range(4):
        occurrence=state['selected']['id'];worker=state['media']['workerPid']
        # The PID is reported by this host's private worker launcher. No name-
        # based kill and no interaction with the original kiosk process tree.
        started=time.monotonic();os.kill(worker,signal.SIGKILL)
        failed=wait(lambda s:s['renderer']=='failed')
        assert failed['instance']==instance and failed['generation']==generation
        assert failed['recovery']['failedOccurrence']==occurrence and failed['presented'] is None
        if attempt==3:
            assert failed['recovery']['exhausted'] and not failed['recovery']['pending']
            time.sleep(4.5)
            state=call('state.get')
            assert state['selected']['id']==occurrence and state['renderer']=='failed'
            evidence['checks'].append({'name':'fourth failure remains terminal with responsive host','state':state['recovery']})
            break
        state=wait(lambda s:s['renderer']=='presented' and s['selected']['id']!=occurrence and (s['media'] or {}).get('workerPid') and s['media'].get('hwdec-current'))
        assert state['instance']==instance and state['generation']==generation
        assert state['recovery']['attempt']==attempt+1
        assert state['media']['hwdec-current']=='vaapi'
        elapsed=time.monotonic()-started
        assert elapsed >= (1<<attempt), elapsed
        evidence['checks'].append({'name':'decoder replacement','attempt':attempt+1,'seconds':elapsed,
            'failedOccurrence':occurrence,'presentedOccurrence':state['presented']['id'],'hardwareDecode':state['media']['hwdec-current']})
    call('playback.refresh')
    state=wait(lambda s:s['renderer']=='presented')
    assert state['recovery']['attempt']==0 and not state['recovery']['exhausted']
    evidence['checks'].append({'name':'explicit refresh recovers after exhausted budget','occurrence':state['selected']['id']})
    evidence['result']='passed'
except Exception as error:
    evidence['result']='failed';evidence['error']=str(error);raise
finally:
    evidence['finishedAt']=time.time()
    with open(args.output,'w') as output:json.dump(evidence,output,indent=2);output.write('\n')
    connection.close()
