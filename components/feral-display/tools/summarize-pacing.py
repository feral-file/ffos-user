#!/usr/bin/env python3
"""Compare matched runs using compositor feedback, never navigation success."""
import argparse
import json
import statistics
from pathlib import Path

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('runs', nargs='+', type=Path)
a = p.parse_args()

def stats(values):
    values = sorted(values)
    if len(values) < 20:
        raise ValueError('Too few output presentation intervals')
    return {'count':len(values), 'meanMs':statistics.mean(values),
            'medianMs':statistics.median(values), 'p95Ms':values[int(len(values)*.95)],
            'p99Ms':values[int(len(values)*.99)], 'over100ms':sum(v>100.5 for v in values)}

runs = []
identity = None
for path in a.runs:
    run = json.loads(path.read_text())
    assert run['result'] == 'measured', path
    current = (run['version']['Browser'].split('/')[-1], run['version']['V8-Version'],
               run['version']['WebKit-Version'], run['fixture']['fixtureSha256'],
               run['fixture']['seed'], run['document']['viewport'], run['document']['renderer'])
    if identity is None:
        identity = current
    assert current == identity, f'Unmatched engine/artwork/backend: {path}'
    start, end = (run[k]['monotonicNs'] for k in ('clockStart','clockEnd'))
    native = 'runtime' in run['unit']
    if native:
        timing = json.loads(path.with_name(path.stem+'-timing.json').read_text())
        feedback = timing['waylandPresentation']
        assert feedback['available'] and feedback['clockId'] == 1, 'Monotonic compositor clock required'
        assert feedback['changedTimestampsNs'][0] <= start, 'Timing ring no longer covers sample start'
        assert feedback['changedTimestampsNs'][-1] >= end-200_000_000, 'Timing ring does not cover sample end'
        timestamps = [t for t in feedback['changedTimestampsNs'] if start <= t <= end]
        notes = {'cpuCopies':timing['cpuCopies'], 'waylandFlags':feedback['lastFlags'],
                 'discardedSubmissionsWholeRun':feedback['discarded']}
    else:
        events = run['compositorTrace']['events']
        assert len({(e['pid'],e['tid']) for e in events}) == 1, 'Multiple output streams need separate analysis'
        timestamps = [e['ts']*1000 for e in events if start <= e['ts']*1000 <= end]
        notes = {'trace':'Display::FrameDisplayed', 'physicalOutputVerified':False}
    intervals = [(b-a)/1e6 for a,b in zip(timestamps,timestamps[1:])]
    assert all(x>0 for x in intervals), 'Non-monotonic presentation timestamps'
    runs.append({'file':path.name,'arm':'native' if native else 'chrome',
                 'outputIntervals':stats(intervals), 'rafTimestamps':run['frameIntervals'],
                 'callbackDelivery':run.get('callbackDelivery'),
                 'cpuPercentOneCore':run['cpuPercentOneCore'],
                 'memoryMiB':run['after']['MemoryCurrent']/2**20,
                 'peakMemoryMiB':run['after']['MemoryPeak']/2**20, 'notes':notes})
print(json.dumps({'physicalOutputVerified':False,'runs':runs},indent=2))
