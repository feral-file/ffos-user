#!/usr/bin/env python3
"""Verify the bundled viewer against its pinned upstream package, retain license."""
import hashlib
import json
from pathlib import Path
import tarfile

root=Path(__file__).resolve().parent.parent
dependencies=json.loads((root/'dependencies.json').read_text())
dependency=dependencies['model_viewer']
archive=root/'vendor'/dependency['file']
assert hashlib.sha256(archive.read_bytes()).hexdigest()==dependency['sha256'], 'model-viewer archive hash mismatch'
with tarfile.open(archive) as package:
    bundle=package.extractfile('package/dist/model-viewer.min.js').read()
    license=package.extractfile('package/LICENSE').read()
assert hashlib.sha256(bundle).hexdigest()==dependency['bundleSha256'], 'model-viewer bundle hash mismatch'
assert (root/'renderers/model-viewer.min.js').read_bytes()==bundle, 'checked-in viewer differs from pinned upstream'
(root/'vendor/model-viewer-LICENSE').write_bytes(license)
print('Bundled model-viewer 4.3.1 matches pinned package; license retained')
for dependency in dependencies.values():
    if 'rendererPath' not in dependency: continue
    source=root/'vendor'/dependency['file']
    content=source.read_bytes()
    if hashlib.sha256(content).hexdigest()!=dependency['sha256']:
        raise SystemExit('Renderer dependency checksum mismatch: '+str(source))
    target=root/'renderers'/dependency['rendererPath']
    target.parent.mkdir(parents=True,exist_ok=True)
    target.write_bytes(content)
print('Pinned Draco and Basis decoder assets packaged locally')
