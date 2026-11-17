#!/usr/bin/env python3
"""Restart the core against an isolated imported state; never claims rendering."""
import argparse
import json
from pathlib import Path
import socket
import sqlite3
import subprocess
import tempfile
import time
from playerctl import exchange

parser=argparse.ArgumentParser()
parser.add_argument('--binary',type=Path,required=True)
parser.add_argument('--state-dir',type=Path,required=True)
parser.add_argument('--output',type=Path,required=True)
args=parser.parse_args()
database=args.state_dir/'state.sqlite'
with sqlite3.connect(f'file:{database}?mode=ro',uri=True) as connection:
    original=json.loads(connection.execute('SELECT document FROM state WHERE id=1').fetchone()[0])
evidence={'result':'pending','runs':[],'sourceSHA256':json.loads((args.state_dir/'migration.json').read_text())['sourceSHA256']}
for _ in range(2):
    with tempfile.TemporaryDirectory(prefix='ff-import-restart-') as directory:
        path=str(Path(directory)/'player.sock')
        process=subprocess.Popen([str(args.binary.resolve()),str(database.resolve()),path])
        try:
            for attempt in range(500):
                if Path(path).exists(): break
                if process.poll() is not None: raise RuntimeError('Imported state failed to load')
                time.sleep(.01)
            with socket.socket(socket.AF_UNIX) as connection:
                connection.settimeout(5);connection.connect(path)
                hello=exchange(connection,{'v':1,'id':'hello','method':'hello','params':{'client':'import-restart-test','versions':[1]}})
                state=hello['snapshot']
                assert state['presented'] is None
                assert state['playlist']==original['playlist']
                for key in ('stopped','sleeping','paused','loop','shuffle','context'):
                    assert state[key]==original[key],key
                for key,value in original['settings'].items(): assert state['settings'][key]==value,key
                if state['selected']: assert state['selected']['stage']=='loading'
                history=exchange(connection,{'v':1,'id':'history','method':'history.list','params':{},'deadlineMs':int(time.time()*1000)+4000})
                assert history['records']==original['history'] and history['activeOccurrence'] is None
                evidence['runs'].append({'instance':state['instance'],'generation':state['generation'],'renderer':state['renderer'],
                    'selectedIndex':state['selected']['index'] if state['selected'] else None,'presented':None,
                    'playlistItems':len(state['playlist']['items']) if state['playlist'] else 0,'historyRecords':len(history['records']),
                    'settingsPreserved':True,'intentPreserved':True})
        finally:
            process.terminate()
            try: process.wait(timeout=5)
            except subprocess.TimeoutExpired: process.kill();process.wait()
assert evidence['runs'][0]['instance']!=evidence['runs'][1]['instance']
evidence['result']='passed'
args.output.write_text(json.dumps(evidence,indent=2)+'\n')
print('Imported state survived two host starts without fabricated presentation/history')
