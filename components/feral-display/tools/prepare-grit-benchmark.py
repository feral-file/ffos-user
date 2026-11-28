#!/usr/bin/env python3
"""Create an auditable benchmark fixture; never used by normal playback."""
import argparse
import hashlib
import json
from pathlib import Path

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('source', type=Path)
p.add_argument('output', type=Path)
p.add_argument('--seed', default='feral-frame-pacing-20261118')
a = p.parse_args()
raw = a.source.read_bytes()
needle = b'i.setSeed(i.getRandomSeed())'
if raw.count(needle) != 1:
    raise SystemExit('Expected exactly one seed-selection call; review changed artwork')
metadata = {'seed': a.seed, 'sourceSha256': hashlib.sha256(raw).hexdigest(),
            'sourceUrl': 'https://cdn.feralfileassets.com/previews/2a727b17-7890-4464-913d-3fe5ad2af4ae/1615474528/?edition_number=38&blockchain=bitmark'}
fixture = raw.replace(needle, ('i.setSeed(' + json.dumps(a.seed).replace('<', '\\u003c') + ')').encode())
# The marker lets the measurement harness refuse the original randomly seeded
# document. This is fixture metadata, not a privileged product renderer bridge.
marker = ('<script>window.__ffBenchmark=' + json.dumps(metadata).replace('<', '\\u003c') + ';</script>').encode()
if fixture.count(b'<body>') != 1:
    raise SystemExit('Expected one body element')
fixture = fixture.replace(b'<body>', b'<body>' + marker)
a.output.parent.mkdir(parents=True, exist_ok=True)
a.output.write_bytes(fixture)
metadata['fixtureSha256'] = hashlib.sha256(fixture).hexdigest()
a.output.with_suffix('.json').write_text(json.dumps(metadata, indent=2) + '\n')
print(json.dumps(metadata))
