#!/usr/bin/env python3
"""Generate synthetic orientation/animation fixtures; no artwork is modified.

Generated with Pillow 12.3.0. The committed JSON contains exact test bytes, so
ordinary device verification does not need Pillow or regenerate encodings.
"""
import base64
import io
import json
from pathlib import Path
from PIL import Image

def encoded(image, format, **options):
    buffer=io.BytesIO()
    image.save(buffer,format=format,**options)
    return base64.b64encode(buffer.getvalue()).decode()

image=Image.new('RGB',(40,20),'red')
for x in range(20,40):
    for y in range(20):image.putpixel((x,y),(0,0,255))
exif=Image.Exif();exif[274]=6
jpeg=encoded(image,'JPEG',quality=100,subsampling=0,exif=exif)
red=Image.new('RGB',(40,20),'red');green=Image.new('RGB',(40,20),(0,255,0))
gif=encoded(red,'GIF',save_all=True,append_images=[green],duration=[400,400],loop=0)
alpha=Image.new('RGBA',(40,20),(0,0,255,128))
png=encoded(alpha,'PNG')
fixtures=[
    {'name':'exif-rotate-6','mimeType':'image/jpeg','source':'data:image/jpeg;base64,'+jpeg,'expectedSize':[20,40]},
    {'name':'animated-gif','mimeType':'image/gif','source':'data:image/gif;base64,'+gif,'expectedSize':[40,20]},
    {'name':'alpha-png','mimeType':'image/png','source':'data:image/png;base64,'+png,'expectedSize':[40,20]},
]
target=Path(__file__).resolve().parents[1]/'tests/image-fixtures.json'
target.write_text(json.dumps(fixtures,indent=2)+'\n')
