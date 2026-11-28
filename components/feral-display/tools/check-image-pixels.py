#!/usr/bin/env python3
"""Inspect synthetic fixture screenshots; this does not compare artwork fidelity."""
import json
import sys
from pathlib import Path
from PIL import Image

root=Path(sys.argv[1]);result=json.loads((root/'result.json').read_text())
def pixel(name,x,y):
    with Image.open(root/name) as image:
        return image.convert('RGB').getpixel((int(image.width*x),int(image.height*y)))
def near(value,target,tolerance=8):
    assert all(abs(a-b)<=tolerance for a,b in zip(value,target)),(value,target)
near(pixel('exif-rotate-6-0.png',.5,.25),(255,0,0))
near(pixel('exif-rotate-6-0.png',.5,.75),(0,0,255))
near(pixel('exif-rotate-6-0.png',.1,.5),(0,0,0))
colors=[pixel(f'animated-gif-{i}.png',.5,.5) for i in range(6)]
assert (255,0,0) in colors and (0,255,0) in colors,colors
near(pixel('alpha-png-0.png',.5,.5),(0,0,128))
with Image.open(root/'alpha-png-0.png') as image:
    # The 2:1 fixture has a narrow letterbox at 4K 16:9 and a wide one
    # on square panels. Sample inside actual padding, not a fixed 10% point.
    scaled_height = min(image.height, image.width / 2)
    scaled_width = scaled_height * 2
    if scaled_height < image.height:
        point = (image.width // 2, int((image.height-scaled_height)/4))
    elif scaled_width < image.width:
        point = (int((image.width-scaled_width)/4), image.height // 2)
    else:
        raise AssertionError('Use a non-2:1 viewport to verify image padding')
    near(image.convert('RGB').getpixel(point),(0,0,0))
result['pixelChecks']=['EXIF 6: red top, blue bottom, pillarbox','GIF: both frames observed','PNG: half-alpha blue over black; letterbox']
result['result']='passed';result['physicalOutputVerified']=False
(root/'result.json').write_text(json.dumps(result,indent=2)+'\n')
print('Synthetic orientation, animation, alpha and aspect-ratio pixel checks passed')
