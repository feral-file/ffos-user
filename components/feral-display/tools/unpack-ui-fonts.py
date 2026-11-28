#!/usr/bin/env python3
"""Reproduce native SFNT faces from the existing product's WOFF assets.

Maintenance only: python -m pip install fonttools==4.61.1
Runtime/CI consume the checked-in files; no font network fetch is required.
"""
from pathlib import Path
from fontTools.ttLib import TTFont

root = Path(__file__).resolve().parent.parent / 'assets/fonts'
for source in sorted(root.glob('*.woff')):
    font = TTFont(source, recalcTimestamp=False)
    font.flavor = None
    destination = source.with_suffix('.ttf')
    font.save(destination)
    decoded = TTFont(destination, recalcTimestamp=False)
    # SFNT's global checksum adjustment changes with the container. Every
    # other table (outlines, names, metrics, kerning, layout) remains identical.
    for tag in font.keys():
        if tag not in ('GlyphOrder', 'head'):
            assert font.getTableData(tag) == decoded.getTableData(tag), tag
    print(destination.name)
