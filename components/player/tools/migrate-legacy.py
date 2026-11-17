#!/usr/bin/env python3
"""Offline, fail-closed legacy export conversion and atomic native-state install."""
import argparse
import ctypes
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import random
import shutil
import subprocess
import sys
import tempfile

LIMIT = 4 * 1024 * 1024
KEYS = {'castInfo', 'displaySettings', 'viewMode', 'criticalTemp', 'dp1_scheduled_tasks',
        'boot_playlist', 'defaultItemDuration', 'recentlyPlayed', 'recentlyPlayedIncomplete', 'contentPolicy'}

def require(condition, message):
    if not condition: raise ValueError(message)

def pairs(values):
    result = {}
    for key, value in values:
        require(key not in result, 'Duplicate JSON key: ' + key)
        result[key] = value
    return result

def decode(text):
    return json.loads(text, object_pairs_hook=pairs,
                      parse_constant=lambda value: (_ for _ in ()).throw(ValueError('Non-finite JSON: ' + value)))

def encode(value):
    return (json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(',', ':')) + '\n').encode()

def context(value):
    require(value in ('curated', 'personal'), 'Unknown content context')
    return value

def flag(value, name):
    require(type(value) is bool, name + ' must be boolean')
    return value

def normalize(export, digest):
    require(export.get('format') == 'ff-player-storage-export' and export.get('version') == 2,
            'A version-2 live export with runtime intent is required; re-export through the old player')
    require(export.get('origin') == 'http://127.0.0.1:8080', 'Unexpected legacy player origin')
    records = export.get('records')
    require(isinstance(records, dict) and set(records) == KEYS, 'Incomplete or unknown exported storage keys')
    def record(key):
        raw = records[key]
        require(raw is None or isinstance(raw, str), 'Legacy storage record must be string/null: ' + key)
        return None if raw is None else decode(raw)
    runtime = export.get('runtime', {})
    require(runtime.get('bootHydration') in ('ok', 'halted_cleared', 'halted_preserving'), 'Legacy hydration was not settled')
    route = runtime.get('route')
    require(route in ('/', '/playlist', '/sleep', '/error'), 'Unsupported legacy route: ' + str(route))
    cast = record('castInfo')
    require(cast is None or isinstance(cast, dict), 'Invalid stored cast')
    # One-shot recovery markers need the native fallback resolver. Until that
    # exists, refuse activation rather than relaunch the resource-heavy work.
    thermal = record('criticalTemp')
    require(thermal is None or type(thermal) is bool, 'Invalid thermal recovery marker')
    require(thermal is not True, 'Thermal fallback is pending; migration awaits native fallback support')
    require(not cast or cast.get('castCommand') != 'castDaily', 'Daily fallback marker requires native fallback support')
    active = runtime.get('cast', {})
    require(isinstance(active, dict), 'Missing live cast observation')
    if flag(runtime.get('hasArtwork'), 'hasArtwork'):
        require(cast is not None and cast.get('playlist') == active.get('playlist'), 'Live playlist differs from durable cast; retry export')
        require(cast.get('index', 0) == active.get('index', 0), 'Live index differs from durable cast; retry export')
    playlist = cast.get('playlist') if cast else None
    index = cast.get('index', 0) if cast else 0
    require(type(index) is int and index >= 0, 'Invalid stored index')
    items = playlist.get('items', []) if isinstance(playlist, dict) else []
    require(playlist is None or isinstance(playlist, dict), 'Invalid stored playlist')
    require((items and index < len(items)) or (not items and index == 0), 'Stored index is outside the playlist')
    stopped = not items or route == '/error' or runtime['bootHydration'] == 'halted_cleared'
    sleeping = route == '/sleep'
    # /error is a preserving halt, not permission to resume the failing work.
    state = {'version':1, 'playlist':playlist, 'playlistUrl':cast.get('playlistUrl', '') if cast else '',
        'index':index, 'context':context(cast.get('contentContext', 'curated') if cast else 'curated'),
        'stopped':stopped, 'sleeping':sleeping, 'paused':flag(cast.get('isPaused', False) if cast else False, 'isPaused'),
        'loop':cast.get('loopMode', 'playlist') if cast else 'playlist',
        'shuffle':flag(cast.get('shuffle', False) if cast else False, 'shuffle'),
        'order':list(range(len(items))), 'settings':{},
        'policy':record('contentPolicy'),
        'history':[], 'historyIncomplete':False,
        'boot':None, 'schedule':None}
    if state['policy'] is None:
        state['policy'] = {'version':1,'showMatureContent':False,'strictPersonal':False,'blockUnratedCurated':False}
    incomplete = record('recentlyPlayedIncomplete')
    if incomplete is not None: state['historyIncomplete'] = flag(incomplete, 'historyIncomplete')
    if state['shuffle']: random.Random(digest).shuffle(state['order'])
    settings = record('displaySettings')
    if settings is not None:
        require(isinstance(settings, dict), 'Invalid display settings')
        require(set(settings) <= {'scaling','framing','tombstone','isSaved'},
                'Legacy per-edge/asset settings require a native mapping; refusing to discard preferences')
        state['settings'] = {key:value for key,value in settings.items() if key != 'isSaved'}
    duration = record('defaultItemDuration')
    if duration is not None: state['settings']['durationSeconds'] = duration
    require(records['viewMode'] in (None, 'landscape', 'portrait'), 'Unknown legacy viewport orientation')
    history = record('recentlyPlayed')
    if history is not None:
        require(isinstance(history, list), 'Invalid history')
        for entry in history:
            require(isinstance(entry, dict), 'Invalid history record')
            state['history'].append({**{k:v for k,v in entry.items() if k != 'contentContext'},
                                     'context':context(entry.get('contentContext', 'curated'))})
    boot = record('boot_playlist')
    if boot is not None:
        require(isinstance(boot, dict), 'Invalid boot playlist')
        state['boot'] = {'playlist':boot.get('playlist', boot), 'context':context(boot.get('contentContext', 'curated'))}
    schedule = record('dp1_scheduled_tasks')
    if schedule is not None:
        require(isinstance(schedule, dict), 'Invalid scheduled task')
        when = datetime.fromisoformat(schedule['scheduleTime'].replace('Z', '+00:00'))
        require(when.tzinfo is not None, 'Schedule time must include a timezone')
        state['schedule'] = {'playlist':schedule['dp1CallData'], 'atMs':int(when.timestamp()*1000),
                             'context':context(schedule.get('contentContext','curated'))}
    return {'format':'ff-player-native-import','version':1,'state':state,'provenance':{
        'sourceFormat':export['format'],'sourceVersion':2,'sourceSHA256':digest,'exportedAt':export['exportedAt'],
        'legacyRoute':route,'legacyViewMode':records['viewMode'],
        'notes':['Legacy viewMode is derived viewport geometry, not rotation.',
                 'No renderer success, showing key, active history record, or verification annotation imported.',
                 'Legacy shuffle order was not persisted; native order is regenerated while retaining the selected index.']}}

def publish_no_replace(source, destination):
    libc = ctypes.CDLL(None, use_errno=True)
    if sys.platform == 'linux':
        rename = libc.renameat2
        rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
        result = rename(-100, os.fsencode(source), -100, os.fsencode(destination), 1) # RENAME_NOREPLACE
    elif sys.platform == 'darwin':
        rename = libc.renamex_np
        rename.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint]
        result = rename(os.fsencode(source), os.fsencode(destination), 4) # RENAME_EXCL
    else: raise RuntimeError('Atomic no-replace publication is supported on Linux and macOS only')
    if result != 0: raise OSError(ctypes.get_errno(), os.strerror(ctypes.get_errno()), str(destination))

def fsync(path):
    descriptor = os.open(path, os.O_RDONLY)
    try: os.fsync(descriptor)
    finally: os.close(descriptor)

def migrate(source, destination, validator):
    body = source.read_bytes()
    require(len(body) <= LIMIT, 'Legacy export exceeds 4 MiB')
    normalized = normalize(decode(body), hashlib.sha256(body).hexdigest())
    payload = encode(normalized)
    require(len(payload) <= LIMIT, 'Normalized import exceeds 4 MiB')
    require(not destination.exists() and not destination.is_symlink(), 'Destination exists; import never overwrites native state')
    parent = destination.parent.resolve(strict=True)
    require(parent.stat().st_uid == os.geteuid(), 'Destination parent must belong to this user')
    staging = Path(tempfile.mkdtemp(prefix='.ff-migration-', dir=parent))
    try:
        document = staging / 'normalized.json'
        document.write_bytes(payload); document.chmod(0o600)
        native = staging / 'native'; native.mkdir(mode=0o700)
        subprocess.run([str(validator.resolve()), str(document), str(native)], check=True, timeout=30)
        # Retain the exact export beside the new state for audit and rollback.
        (native / 'legacy-export.json').write_bytes(body)
        for child in native.iterdir():
            require(child.is_file() and not child.is_symlink(), 'Unexpected staging output')
            child.chmod(0o600); fsync(child)
        fsync(native)
        publish_no_replace(native, parent / destination.name)
        fsync(parent)
    finally: shutil.rmtree(staging)
    return normalized['provenance']

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('export', type=Path)
    parser.add_argument('destination', type=Path)
    parser.add_argument('--validator', type=Path, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    provenance = migrate(args.export, args.destination, args.validator)
    print(json.dumps({'result':'imported','destination':str(args.destination),'sourceSHA256':provenance['sourceSHA256']}))

if __name__ == '__main__':
    try: main()
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError) as error:
        raise SystemExit('Migration failed; existing player/native state was not overwritten: ' + str(error)) from error
