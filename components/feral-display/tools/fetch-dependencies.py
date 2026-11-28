#!/usr/bin/env python3
"""Fetch only pinned archives; never trust a partial or changed upstream file."""
import sys
import hashlib
import json
import pathlib
import subprocess

root = pathlib.Path(__file__).resolve().parent.parent
vendor = root / 'vendor'
vendor.mkdir(exist_ok=True)
for name, dependency in json.loads((root / 'dependencies.json').read_text()).items():
    if "--core-only" in sys.argv and name != "nlohmann_json": continue
    if name=="cef" and "--without-cef" in sys.argv: continue
    target = vendor / dependency['file']
    def matches(path):
        if not path.is_file():
            return False
        digest = hashlib.sha256()
        with path.open('rb') as stream:
            while chunk := stream.read(1024 * 1024):
                digest.update(chunk)
        return digest.hexdigest() == dependency['sha256']
    if matches(target):
        print(f'{name}: verified existing archive', flush=True)
        continue
    temporary = target.with_suffix(target.suffix + '.part')
    subprocess.run(['curl', '--fail', '--location', '--retry', '3', '--connect-timeout', '15',
                    '--max-time', '900', '--output', str(temporary), dependency['url']], check=True)
    if not matches(temporary):
        temporary.unlink()
        raise SystemExit(f'{name}: checksum mismatch; refusing dependency')
    temporary.replace(target)
    print(f'{name}: downloaded and verified', flush=True)
