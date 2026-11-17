#!/usr/bin/env python3
"""Destructive playback checks for an isolated native test instance, never legacy CDP."""
import argparse
import json
import os
import signal
import socket
import time
import uuid
from playerctl import exchange

parser = argparse.ArgumentParser()
parser.add_argument('--socket', required=True)
parser.add_argument('--output', required=True)
args = parser.parse_args()
connection = socket.socket(socket.AF_UNIX)
connection.settimeout(5)
connection.connect(args.socket)
evidence = {'checks':[], 'startedAt':time.time()}
def call(method, params=None):
    hello = exchange(connection, {'v':1,'id':str(uuid.uuid4()),'method':'hello',
                                  'params':{'client':'device-media-check','versions':[1]}})
    return exchange(connection, {'v':1,'id':str(uuid.uuid4()),'method':method,'params':params or {},
                                 'instance':hello['instance'],'generation':hello['snapshot']['generation'],
                                 'deadlineMs':int(time.time()*1000)+4000,'key':str(uuid.uuid4())})
def wait(predicate, seconds=15):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        state = call('state.get')
        if predicate(state):
            return state
        if state['renderer'] == 'failed':
            raise AssertionError(state['selected']['error'])
        time.sleep(0.1)
    raise TimeoutError(json.dumps(state))
def cast(filename, display=None):
    result = call('playback.replace', {'playlist':{'title':'Native media verification','items':[
        {'id':filename,'title':filename,'source':'http://127.0.0.1:18990/'+filename,
         'duration':0,'display':display or {'loop':True}}]}})
    assert result['outcome']=='accepted'
    return wait(lambda s:s['renderer']=='presented' and (s['media'] or {}).get('importedFrames',0)>0)
def record(name, state):
    evidence['checks'].append({'name':name,'instance':state['instance'], 'generation':state['generation'],
                               'renderer':state['renderer'],'media':state['media'],
                               'selected':state['selected']['id'] if state['selected'] else None})
try:
    state = cast('media-4k-h264.mp4')
    state = wait(lambda s:(s['media'] or {}).get('hwdec-current')=='vaapi' and s['media'].get('seekable') is True)
    instance = state['instance']
    occurrence = state['presented']['id']
    record('4K H.264 hardware decode and host import', state)
    call('playback.pause', {'paused':True})
    time.sleep(1.2)
    before = call('state.get')['media']['time-pos']
    time.sleep(1.3)
    after = call('state.get')['media']['time-pos']
    assert abs(after-before)<0.05, (before,after)
    call('media.seek', {'occurrence':occurrence,'seconds':2})
    state = wait(lambda s:abs((s['media'] or {}).get('time-pos',-100)-2)<0.1)
    assert state['presented']['id']==occurrence and state['renderer']=='presented'
    record('pause and exact seek preserve presented occurrence', state)
    call('playback.pause', {'paused':False})
    state = wait(lambda s:(s['media'] or {}).get('time-pos',0)>2.4)
    record('resume advances media position', state)
    worker = state['media']['workerPid']
    os.kill(worker, signal.SIGKILL)
    state = wait(lambda s:s['renderer']=='failed')
    assert state['host']=='responsive' and state['instance']==instance
    assert state['presented'] is None
    record('decoder termination leaves host and command interface responsive', state)
    state = cast('media-4096-square-h264.mp4')
    state = wait(lambda s:(s['media'] or {}).get('hwdec-current') in ('no',''))
    record('4096-square H.264 software fallback', state)
    state = cast('media-h264-aac.mp4', {'loop':False,'autoPlay':False})
    assert state['mediaPaused'] is True
    time.sleep(1.2)
    before = call('state.get')['media']['time-pos']
    time.sleep(1.2)
    assert abs(call('state.get')['media']['time-pos']-before)<0.05
    record('autoPlay false holds first frame', state)
    call('playback.pause', {'paused':False})
    state = wait(lambda s:(s['media'] or {}).get('time-pos',0)>0.4)
    record('explicit resume releases autoPlay hold', state)
    items = [{'id':str(i),'source':'http://127.0.0.1:18990/media-h264-aac.mp4','duration':0,
              'display':{'loop':False}} for i in range(2)]
    call('playback.replace', {'playlist':{'items':items}})
    call('playback.mode', {'loop':'none'})
    state = wait(lambda s:s['renderer']=='presented')
    first = state['presented']['id']
    call('media.seek', {'occurrence':first,'seconds':3.5})
    state = wait(lambda s:s['renderer']=='presented' and s['presented']['index']==1)
    assert state['presented']['id']!=first
    record('EOF advances to a distinct occurrence with identical URL',state)
    state = cast('hls/index.m3u8')
    state = wait(lambda s:(s['media'] or {}).get('hwdec-current')=='vaapi' and s['media'].get('time-pos',0)>0.3)
    record('HLS VOD decode and host composition',state)
    state = cast('audio.m4a')
    state = wait(lambda s:(s['media'] or {}).get('current-ao')=='pulse' and s['media'].get('time-pos',0)>0.3)
    record('AAC audio output connected to PulseAudio; audibility not established',state)
    call('settings.set',{'volume':0.25,'muted':True})
    state = wait(lambda s:(s['media'] or {}).get('volume')==25 and s['media'].get('mute') is True)
    record('native volume and mute applied',state)
    state = cast('audio.mp3')
    state = wait(lambda s:(s['media'] or {}).get('current-ao')=='pulse' and s['media'].get('time-pos',0)>0.3)
    record('MP3 audio output connected to PulseAudio',state)
    worker = state['media']['workerPid']
    # Stop the actual decoder (namespace child), not bwrap's waiting parent.
    children = open(f'/proc/{worker}/task/{worker}/children').read().split()
    assert len(children)==1, children
    decoder = int(children[0])
    grandchildren = open(f'/proc/{decoder}/task/{decoder}/children').read().split()
    if grandchildren:
        assert len(grandchildren)==1, grandchildren
        decoder = int(grandchildren[0])
    os.kill(decoder, signal.SIGSTOP)
    state = wait(lambda s:s['renderer']=='failed', seconds=8)
    assert state['host']=='responsive' and state['instance']==instance
    assert 'stopped responding' in state['selected']['error']
    record('decoder hang bounded by heartbeat deadline',state)
    evidence['result']='passed'
except Exception as error:
    evidence['result']='failed'
    evidence['error']=str(error)
    raise
finally:
    evidence['finishedAt']=time.time()
    with open(args.output,'w') as output:
        json.dump(evidence,output,indent=2)
        output.write('\n')
    connection.close()
