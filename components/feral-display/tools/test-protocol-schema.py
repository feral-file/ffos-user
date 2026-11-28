#!/usr/bin/env python3
"""Validate real C++ replies and client requests against the canonical schema."""
import argparse
import json
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import uuid

from jsonschema import Draft202012Validator
from playerctl import exchange, receive

parser = argparse.ArgumentParser()
parser.add_argument('binary', type=Path)
args = parser.parse_args()
root = Path(__file__).resolve().parent.parent
schema = json.loads((root / 'protocol/schema.json').read_text())
Draft202012Validator.check_schema(schema)
validator = Draft202012Validator(schema)

def check(value, definition):
    Draft202012Validator({**schema, 'oneOf': [{'$ref': '#/$defs/' + definition}]}).validate(value)

with tempfile.TemporaryDirectory(prefix='ff-schema-') as directory:
    path = str(Path(directory) / 'player.sock')
    process = subprocess.Popen([str(args.binary.resolve()), str(Path(directory) / 'state.sqlite'), path])
    try:
        for _ in range(500):
            if Path(path).exists(): break
            if process.poll() is not None: raise RuntimeError('C++ host failed to start')
            time.sleep(.01)
        with socket.socket(socket.AF_UNIX) as connection:
            connection.settimeout(5)
            connection.connect(path)
            hello = {'v': 1, 'id': 'hello', 'method': 'hello', 'params': {'client': 'schema-test', 'versions': [1]}}
            validator.validate(hello)
            state = exchange(connection, hello)
            check(state, 'hello')
            instance = state['instance']
            generation = state['snapshot']['generation']

            def call(method, params, definition):
                global generation
                request = {'v': 1, 'id': str(uuid.uuid4()), 'method': method, 'params': params,
                           'instance': instance, 'generation': generation, 'key': str(uuid.uuid4()),
                           'deadlineMs': int(time.time() * 1000) + 4000}
                validator.validate(request)
                result = exchange(connection, request)
                validator.validate({'v': 1, 'id': request['id'], 'result': result})
                check(result, definition)
                generation = result.get('generation', generation)
                return result

            playlist = {'title': 'Schema corpus', 'items': [{'source': 'https://example.org/work', 'display': {'margin': '.5%'}}]}
            calls = [
                ('state.get', {}, 'snapshot'), ('health.get', {}, 'snapshot'),
                ('playback.replace', {'playlist': playlist}, 'receipt'),
                ('state.get', {}, 'snapshot'), ('health.get', {}, 'snapshot'),
                ('playback.update', {'items': playlist['items']}, 'receipt'),
                ('playback.pause', {'paused': True}, 'receipt'),
                ('playback.pause', {'paused': False}, 'receipt'),
                ('playback.select', {'index': 0}, 'receipt'),
                ('playback.mode', {'loop': 'one', 'shuffle': True}, 'receipt'),
                ('settings.set', {'durationSeconds': None, 'volume': .5, 'rotation': 90}, 'receipt'),
                ('artworkPermissions.get', {}, 'artworkPermissions'),
                ('artworkPermissions.set', {'notifications': False}, 'receipt'),
                ('policy.get', {}, 'policy'),
                ('policy.set', {'version': 1, 'showMatureContent': True, 'strictPersonal': False, 'blockUnratedCurated': False}, 'receipt'),
                ('history.list', {}, 'history'),
                ('connectivity.set', {'online': False}, 'receipt'),
                ('overlay.set', {'owner': 'setup', 'state': 'joining', 'fields': {'ssid': 'network'}}, 'receipt'),
                ('overlay.set', {'owner': 'setup', 'state': 'hidden'}, 'receipt'),
                ('schedule.set', {'playlist': playlist, 'atMs': int(time.time()*1000)+60000}, 'receipt'),
                ('schedule.set', {'clear': True}, 'receipt'),
                ('boot.set', {'playlist': playlist}, 'receipt'), ('boot.set', {'clear': True}, 'receipt'),
                ('playback.sleep', {'sleeping': True}, 'receipt'), ('playback.sleep', {'sleeping': False}, 'receipt'),
                ('playback.refresh', {}, 'receipt'), ('playback.stop', {}, 'receipt'),
                ('playback.cancel', {}, 'receipt'), ('state.get', {}, 'snapshot'),
            ]
            for method, params, definition in calls:
                call(method, params, definition)
            initial = call('state.subscribe', {}, 'subscription')
            # Events travel on a dedicated connection in clients. Mutate from
            # another connection so there is no response/event ordering guess.
            with socket.socket(socket.AF_UNIX) as writer:
                writer.settimeout(5); writer.connect(path)
                stamp = exchange(writer, hello)['snapshot']
                exchange(writer, {'v':1, 'id':'change', 'method':'settings.set', 'params':{'muted':True},
                                  'instance':stamp['instance'], 'generation':stamp['generation'],
                                  'key':'change', 'deadlineMs':int(time.time()*1000)+4000})
            event = receive(connection)
            validator.validate(event)
            assert event['previous'] == initial['cursor'] and event['snapshot']['settings']['muted']
            print(f'Schema validated {len(calls)+3} real C++ exchanges and a subscription event')
    finally:
        process.terminate()
        try: process.wait(timeout=5)
        except subprocess.TimeoutExpired: process.kill(); process.wait()
