#!/usr/bin/env python3
"""Read-only PSS snapshot for an explicitly named isolated benchmark unit."""
import json
from pathlib import Path
import re
import sys
import time

unit=sys.argv[1]
assert re.fullmatch(r'ff-native-(runtime|chrome)-test\.service',unit)
group=Path('/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice')/unit
assert group.is_dir(), group
pids=sorted({int(pid) for p in group.rglob('cgroup.procs') for pid in p.read_text().split()})
assert pids
rows=[]
for pid in pids:
    try:
        stat=Path(f'/proc/{pid}/smaps_rollup').read_text()
        values={line.split(':')[0]:int(line.split()[1]) for line in stat.splitlines()[1:] if ':' in line}
        rows.append({'pid':pid,'name':Path(f'/proc/{pid}/comm').read_text().strip(),
                     **{k:values.get(k,0) for k in ['Pss','Rss','Private_Clean','Private_Dirty','SwapPss']}})
    except FileNotFoundError:
        raise SystemExit('Process exited during snapshot; repeat after stabilization')
print(json.dumps({'unit':unit,'atNs':time.time_ns(),'processes':rows,
                  'totalsKiB':{k:sum(r[k] for r in rows) for k in ['Pss','Rss','Private_Clean','Private_Dirty','SwapPss']}},indent=2))
