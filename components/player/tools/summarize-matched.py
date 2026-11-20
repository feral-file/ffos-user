#!/usr/bin/env python3
"""Summarize paired Chrome trace/native Wayland output in the same clock window."""
import argparse
import json
import statistics
from pathlib import Path

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('output', type=Path)
p.add_argument('runs', type=Path, nargs='+')
a = p.parse_args()

def intervals(timestamps):
    values = [(b - a) / 1e6 for a, b in zip(timestamps, timestamps[1:])]
    if any(v <= 0 for v in values):
        raise ValueError('Non-monotonic presentation timestamps')
    values.sort()
    if len(values) < 10:
        raise ValueError('Too few output samples')
    return dict(count=len(values), meanMs=statistics.mean(values),
                medianMs=statistics.median(values), p95Ms=values[int(len(values)*.95)],
                p99Ms=values[int(len(values)*.99)], over100ms=sum(v > 100.5 for v in values))

result = {'physicalOutputVerified': False, 'runs': []}
reference = None
for path in a.runs:
    data = json.loads(path.read_text())
    if data['result'] != 'measured':
        raise ValueError(f'{path}: run did not complete')
    identity = (data['version']['Browser'].split('/')[-1],
                data['version']['V8-Version'], data['version']['WebKit-Version'],
                data['fixture'], data['document'])
    # Document identity excludes no fields: viewport, canvas, renderer, visibility
    # and seed marker must agree, not only the requested URL or engine pin.
    if reference is None:
        reference = identity
    elif identity != reference:
        raise ValueError(f'{path}: unmatched engine, fixture or rendered document')
    low, high = data['clockStart']['monotonicNs'], data['clockEnd']['monotonicNs']
    presentation_path = data.get('configuration', {}).get('presentationPath')
    if presentation_path in ('cef', 'shipping'):
        arm = presentation_path
    elif presentation_path == 'public' or 'public' in data['unit']:
        arm = 'native'
    else:
        arm = 'native' if 'runtime' in data['unit'] else 'views' if 'views' in data['unit'] else 'chrome'
    copies = None
    if arm not in ('native', 'cef'):
        events = data['compositorTrace']['events']
        if len({(e['pid'], e['tid']) for e in events}) != 1:
            raise ValueError('Multiple output streams need separate analysis')
        times = [e['ts'] * 1000 for e in events]
    else:
        timing = json.loads(path.with_suffix('.timing.json').read_text())
        feedback = timing['waylandPresentation']
        if not feedback['available'] or feedback['clockId'] != 1 or feedback.get('truncatedTimestamps', False):
            raise ValueError('Native feedback needs complete CLOCK_MONOTONIC samples')
        times = feedback['changedTimestampsNs']
        if not times or times[0] > low:
            raise ValueError('Native feedback does not cover sample start')
        copies = {key: timing[key] for key in ('gpuCopies', 'cpuCopies', 'coalescedFrames')}
    # A short prefix must not be compared with CPU statistics for a full run.
    # Two hundred milliseconds allows the measured slow fixture's final frame;
    # longer gaps need separate investigation, not a silently shortened sample.
    if not times or times[0] > low + 200_000_000 or times[-1] < high - 200_000_000:
        raise ValueError('Output feedback does not cover the measurement window')
    if high <= low:
        raise ValueError('Invalid measurement window')
    measured = intervals([t for t in times if low <= t <= high])
    row = dict(file=path.name, arm=arm, outputIntervals=measured,
               rafTimestamps=data['frameIntervals'], callbackDelivery=data['callbackDelivery'],
               cpuPercentOneCore=data['cpuPercentOneCore'],
               cgroupMemoryMiB=data['after']['MemoryCurrent']/1048576,
               cgroupPeakMiB=data['after']['MemoryPeak']/1048576)
    if copies is not None:
        row['wholeRunCopiesIncludingWarmup'] = copies
    pss = path.with_suffix('.pss.json')
    if pss.exists():
        row['processMemory'] = json.loads(pss.read_text())
    result['runs'].append(row)
a.output.write_text(json.dumps(result, indent=2)+'\n')
print(json.dumps([{k:r[k] for k in ('file','outputIntervals','cpuPercentOneCore','cgroupMemoryMiB')} for r in result['runs']],indent=2))
