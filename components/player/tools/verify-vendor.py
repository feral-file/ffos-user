#!/usr/bin/env python3
"""Offline verification used by packaging; never falls back to downloading."""
import hashlib
import json
from pathlib import Path

root=Path(__file__).resolve().parent.parent
for name,dependency in json.loads((root/'dependencies.json').read_text()).items():
    with (root/'vendor'/dependency['file']).open('rb') as source:
        digest=hashlib.file_digest(source,'sha256').hexdigest()
    if digest!=dependency['sha256']: raise SystemExit(name+': dependency checksum mismatch')
print('All pinned dependencies verified offline')
