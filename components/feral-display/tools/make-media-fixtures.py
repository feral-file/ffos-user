#!/usr/bin/env python3
"""Generate local decode/streaming fixtures for device-media-check.py (FFmpeg CLI)."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess

parser = argparse.ArgumentParser()
parser.add_argument('directory', type=Path)
args = parser.parse_args()
root = args.directory.resolve()
root.mkdir(parents=True, exist_ok=True)
source = Path(__file__).resolve().parent.parent / 'tests/media-h264-aac.mp4'
version = subprocess.check_output(['ffmpeg', '-version'], text=True).splitlines()[0]
commands = []

def generate(arguments):
    command = ['ffmpeg', '-hide_banner', '-loglevel', 'error', '-nostdin', '-y', *arguments]
    subprocess.run(command, check=True)
    commands.append(command)

for name, size in [('4k', '3840x2160'), ('4096-square', '4096x4096')]:
    generate(['-f', 'lavfi', '-i', f'testsrc2=size={size}:rate=30', '-t', '4',
              '-c:v', 'libx264', '-threads', '2', '-preset', 'ultrafast', '-crf', '28',
              '-pix_fmt', 'yuv420p', '-movflags', '+faststart', str(root / f'media-{name}-h264.mp4')])
(root / 'hls').mkdir(exist_ok=True)
generate(['-i', str(source), '-c', 'copy', '-hls_time', '1', '-hls_list_size', '0',
          '-hls_segment_filename', str(root / 'hls/segment-%03d.ts'), str(root / 'hls/index.m3u8')])
for extension, codec in [('m4a', 'aac'), ('mp3', 'libmp3lame')]:
    generate(['-i', str(source), '-vn', '-c:a', codec, str(root / f'audio.{extension}')])
files = sorted(p for p in root.rglob('*') if p.is_file() and p.name != 'fixture-manifest.json')
manifest = {'ffmpeg': version, 'commands': commands,
            'files': {str(p.relative_to(root)): hashlib.sha256(p.read_bytes()).hexdigest() for p in files}}
(root / 'fixture-manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
print(f'Generated {len(files)} files in {root}; fixture-manifest.json records provenance')
