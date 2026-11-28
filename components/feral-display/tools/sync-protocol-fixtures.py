#!/usr/bin/env python3
"""Copy/check the canonical conformance fixture used by both repositories."""
import argparse
from pathlib import Path

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser()
parser.add_argument('--controld-root', type=Path, default=root.parent / 'feral-controld')
parser.add_argument('--check', action='store_true')
args = parser.parse_args()
canonical = (root / 'protocol/fixtures.json').read_bytes()
target = args.controld_root / 'playerlink/testdata/fixtures.json'
if args.check:
    if target.read_bytes() != canonical:
        raise SystemExit('Go conformance fixtures differ from the native protocol source')
else:
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(canonical)
print('Go/C++ conformance fixture bytes match')
