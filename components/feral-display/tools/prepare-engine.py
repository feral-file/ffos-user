#!/usr/bin/env python3
"""Unpack the checksum-pinned public engine, retaining executable permissions."""
import hashlib
import json
from pathlib import Path
import shutil
import zipfile

root=Path(__file__).resolve().parents[1]
dep=json.loads((root/'dependencies.json').read_text())['chromium']
vendor=root/'vendor';archive=vendor/dep['file'];target=vendor/'chrome-linux64'
if target.exists() and (target/'.archive-sha256').is_file() and (target/'.archive-sha256').read_text().strip()==dep['sha256']:
    if hashlib.file_digest((target/'chrome').open('rb'),'sha256').hexdigest()==dep['executableSha256']:
        raise SystemExit(0)
with archive.open('rb') as source:
    if hashlib.file_digest(source,'sha256').hexdigest()!=dep['sha256']:
        raise SystemExit('Public engine archive checksum mismatch')
if target.exists():shutil.rmtree(target)
with zipfile.ZipFile(archive) as source:
    for entry in source.infolist():
        path=Path(entry.filename)
        if path.is_absolute() or '..' in path.parts or path.parts[0]!='chrome-linux64':
            raise SystemExit('Unexpected engine archive path')
        source.extract(entry,vendor)
        mode=(entry.external_attr>>16)&0o777
        if mode:(vendor/path).chmod(mode)
(target/'.archive-sha256').write_text(dep['sha256']+'\n')
